package metric

import (
	"context"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"
)

func TestTelemetryLegacyRawRebuildsNonNestedRetainedGrid(t *testing.T) {
	s, e := telemetryFixture(t, "")
	ctx := context.Background()
	now := time.Date(2025, 4, 15, 12, 0, 0, 0, time.UTC)
	at := now.Add(-20 * 24 * time.Hour).Truncate(3 * time.Hour)
	if err := s.CreateMetric(ctx, Definition{Name: "cpu", RetentionDays: 30}); err != nil {
		t.Fatal(err)
	}
	var points []Point
	buckets := map[rollupKey]*rollupBucket{}
	for i := 0; i < 1000; i++ {
		p := Point{MetricName: "cpu", EntityID: "node", Timestamp: at.Add(time.Duration(i) * 10 * time.Second), Value: math.Sin(float64(i)*.71)*40 + 50}
		points = append(points, p)
		key := rollupKey{entityID: "node", bucket: p.Timestamp.Truncate(45 * time.Minute).UnixNano()}
		if buckets[key] == nil {
			buckets[key] = newRollupBucket(100)
			buckets[key].tagsJSON = "{}"
		}
		buckets[key].addPoint(p.Value, p.Timestamp.UnixNano())
	}
	if err := s.WriteBatch(ctx, points); err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.writeRollupBucketsTx(ctx, "cpu", 45*time.Minute, buckets, tx); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec("INSERT INTO "+e.table("progress")+"(task,value) VALUES('legacy_migration_now',?)", now.UnixNano()); err != nil {
		t.Fatal(err)
	}
	cfg := e.cfg
	if err = e.Close(); err != nil {
		t.Fatal(err)
	}
	if err = s.MigrateLegacy(ctx, cfg, nil); err != nil {
		t.Fatal(err)
	}
	if err = s.EnableTelemetry(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	q := AggregateQuery{Query: Query{MetricName: "cpu", EntityID: "node", Start: at, End: at.Add(3*time.Hour - time.Nanosecond)}, Interval: 2 * time.Hour, Aggregation: AggAvg, PreserveSeries: true}
	want, err := AggregatePoints(points, q)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Telemetry().Chart(ctx, q, now)
	if err != nil || len(got.Gaps) != 0 || len(got.Points) != len(want) {
		t.Fatalf("recoverable original observations became coarse chart gaps: %+v %v", got, err)
	}
	for i, p := range got.Points {
		if p.Count != want[i].Count || math.Abs(p.Value-want[i].Value) > 1e-8 {
			t.Fatalf("non-nested raw reconstruction changed population: %+v want %+v", p, want[i])
		}
	}
}

func TestTelemetryChartStreamsLegacyOwnersWithinMemoryBudget(t *testing.T) {
	_, e := telemetryFixture(t, "")
	ctx := context.WithValue(context.Background(), telemetryDistributionStatsKey{}, true)
	now := time.Date(2025, 4, 15, 12, 0, 0, 0, time.UTC)
	at := now.Add(-6 * time.Hour)
	for owner := 0; owner < 32; owner++ {
		var cells []telemetryMigrationCell
		for i := 0; i < 64; i++ {
			observed := at.Add(time.Duration(i) * 10 * time.Second)
			var st telemetryStats
			for n := 0; n < 256; n++ {
				st.add(Point{MetricName: "cpu", Timestamp: observed.Add(time.Duration(n) * time.Millisecond), Value: float64(n % 8)}, int64(owner+1), n)
			}
			cells = append(cells, telemetryMigrationCell{series: telemetryMigrationKey("cpu\x00node\x00{}"), tags: "{}", bucket: observed.UnixNano(), resolution: 10 * time.Second, stats: st})
		}
		if err := e.commitLegacyCells(ctx, fmt.Sprintf("owner-%d", owner), "cpu", "node", cells); err != nil {
			t.Fatal(err)
		}
	}
	q := AggregateQuery{Query: Query{MetricName: "cpu", EntityID: "node", Start: at, End: at.Add(time.Hour - time.Nanosecond)}, Interval: time.Hour, Aggregation: AggAvg, PreserveSeries: true}
	if _, err := e.ChartBatchBounded(ctx, []AggregateQuery{q}, now, 10, 1536); !errors.Is(err, ErrTelemetryBackpressure) {
		t.Fatal("insufficient resident budget was not rejected", err)
	}
	for _, aggregation := range []Aggregation{AggAvg, AggStdDev} {
		q.Aggregation = aggregation
		got, err := e.ChartBatchBounded(ctx, []AggregateQuery{q}, now, 10, 2<<20)
		want := 3.5
		if aggregation == AggStdDev {
			want = math.Sqrt(5.25)
		}
		if err != nil || len(got) != 1 || len(got[0].Points) != 1 || len(got[0].Gaps) != 0 || got[0].Points[0].Count != 32*64*256 || math.Abs(got[0].Points[0].Value-want) > 1e-8 {
			t.Fatalf("ordered owner streaming lost bounded statistics: %+v %v", got, err)
		}
	}
}

func TestTelemetryChartRefundsRecompressedStatistics(t *testing.T) {
	const limit = 8 << 20
	for _, operation := range []string{"add", "merge"} {
		t.Run(operation, func(t *testing.T) {
			budget := &telemetryChartBudget{points: 10, bytes: limit}
			ctx := context.WithValue(context.Background(), telemetryChartBudgetKey{}, budget)
			var dst, src telemetryStats
			at := time.Date(2025, 4, 15, 12, 0, 0, 0, time.UTC)
			for i := 0; i < 256; i++ {
				src.add(Point{MetricName: "cpu", Timestamp: at, Value: float64(i % 8)}, 1, i)
			}
			var shrank bool
			for i := 0; i < 2048; i++ {
				before := chartStatsBytes(&dst)
				var err error
				if operation == "add" {
					err = addChartStats(ctx, &dst, Point{MetricName: "cpu", Timestamp: at, Value: float64(i % 8)}, 1, i)
				} else {
					err = mergeChartStats(ctx, &dst, src)
				}
				if err != nil {
					t.Fatal(err)
				}
				after := chartStatsBytes(&dst)
				shrank = shrank || after < before
				if budget.bytes != limit-after {
					t.Fatalf("released centroids remain charged after %s %d: available %d, retained %d", operation, i, budget.bytes, after)
				}
			}
			if !shrank {
				t.Fatal("fixture never exercised a shrinking sketch")
			}
		})
	}
}

func TestTelemetryChartScalarProjectionPreservesDistributionConsumers(t *testing.T) {
	_, e := telemetryFixture(t, "")
	ctx := context.Background()
	now := time.Date(2025, 4, 15, 12, 0, 0, 0, time.UTC)
	at := now.Add(-time.Hour)
	batch := telemetryBatch("scalar-projection", at, 1, 2, 3, 4, 5)
	for i, value := range []float64{1, 3, -1, 5, 7, -1} {
		batch.Points = append(batch.Points, Point{MetricName: sqliteMergedPingLatencyMetric, EntityID: "node", Timestamp: at.Add(time.Duration(i) * time.Second), Value: value})
	}
	if _, err := e.Accept(ctx, batch, now); err != nil {
		t.Fatal(err)
	}
	summarizeAll(t, e)
	q := AggregateQuery{Query: Query{MetricName: "cpu", EntityID: "node", Start: at, End: at.Add(time.Minute - time.Nanosecond)}, Interval: time.Minute, Aggregation: AggAvg, PreserveSeries: true}
	for _, aggregation := range []Aggregation{AggAvg, AggStdDev, AggP50} {
		q.Aggregation = aggregation
		got, err := e.Chart(ctx, q, now)
		want := 3.0
		if aggregation == AggStdDev {
			want = math.Sqrt(2)
		}
		if err != nil || len(got.Points) != 1 || len(got.Gaps) != 0 || got.Points[0].Count != 5 || math.Abs(got.Points[0].Value-want) > 1e-8 {
			t.Fatalf("scalar projection changed %s: %+v %v", aggregation, got, err)
		}
		for _, stats := range got.stats {
			if aggregation != AggP50 && chartStatsBytes(stats) != 0 {
				t.Fatal("scalar output retained unused distribution")
			}
			if aggregation == AggP50 && chartStatsBytes(stats) == 0 {
				t.Fatal("quantile output lost its distribution")
			}
		}
	}
	card, err := e.PingSummary(ctx, q, now)
	if err != nil || len(card.Avg) != 1 || len(card.P50) != 1 || len(card.StdDev) != 1 || len(card.Loss) != 1 {
		t.Fatalf("statistics card lost its distribution: %+v %v", card, err)
	}
	if card.Avg[0].Count != 6 || math.Abs(card.Avg[0].Value-4) > 1e-8 || math.Abs(card.P50[0].Value-4) > 1e-8 || math.Abs(card.StdDev[0].Value-math.Sqrt(5)) > 1e-8 || math.Abs(card.Loss[0].Value-1.0/3) > 1e-8 {
		t.Fatalf("statistics card changed population or quantiles: %+v", card)
	}
}

func TestTelemetryPingSummaryStreamsBucketsIntoTaskStatistics(t *testing.T) {
	_, e := telemetryFixture(t, "")
	ctx := context.Background()
	now := time.Date(2025, 4, 15, 12, 0, 0, 0, time.UTC)
	at := now.Add(-6 * time.Hour)
	var cells []telemetryMigrationCell
	for i := 0; i < 128; i++ {
		observed := at.Add(time.Duration(i) * time.Minute)
		var st telemetryStats
		for n, value := range []float64{1, 3, -1} {
			st.add(Point{MetricName: sqliteMergedPingLatencyMetric, Timestamp: observed.Add(time.Duration(n) * time.Second), Value: value}, 1, n)
		}
		cells = append(cells, telemetryMigrationCell{series: telemetryMigrationKey("ping.latency_ms\x00node\x00{}"), tags: "{}", bucket: observed.UnixNano(), resolution: time.Minute, stats: st})
	}
	if err := e.commitLegacyCells(ctx, "card-buckets", sqliteMergedPingLatencyMetric, "node", cells); err != nil {
		t.Fatal(err)
	}
	q := AggregateQuery{Query: Query{EntityID: "node", Start: at, End: at.Add(128*time.Minute - time.Nanosecond)}, Interval: time.Minute, PreserveSeries: true}
	budget := &telemetryChartBudget{points: 1, bytes: 2 << 20}
	card, err := e.PingSummary(context.WithValue(ctx, telemetryChartBudgetKey{}, budget), q, now)
	if err != nil || len(card.Avg) != 1 || len(card.P50) != 1 || len(card.StdDev) != 1 || len(card.Loss) != 1 {
		t.Fatalf("card kept per-bucket distributions instead of one task: %+v %v", card, err)
	}
	if card.Avg[0].Count != 384 || math.Abs(card.Avg[0].Value-2) > 1e-8 || math.Abs(card.P50[0].Value-2) > 1e-8 || math.Abs(card.StdDev[0].Value-1) > 1e-8 || math.Abs(card.Loss[0].Value-1.0/3) > 1e-8 || budget.points != 0 || budget.bytes < 0 {
		t.Fatalf("bounded card changed original population: %+v budget %+v", card, budget)
	}
	for i := range cells {
		cells[i].series = telemetryMigrationKey("ping.latency_ms\x00node\x00{\"task_id\":\"extra\"}")
		cells[i].tags = `{"task_id":"extra"}`
	}
	if err := e.commitLegacyCells(ctx, "card-other-task", sqliteMergedPingLatencyMetric, "node", cells); err != nil {
		t.Fatal(err)
	}
	budget = &telemetryChartBudget{points: 1, bytes: 2 << 20}
	if _, err := e.PingSummary(context.WithValue(ctx, telemetryChartBudgetKey{}, budget), q, now); !errors.Is(err, ErrTelemetryBackpressure) {
		t.Fatalf("task cardinality escaped the explicit caller budget: %v", err)
	}
}
