package metric

import (
	"context"
	"math"
	"path/filepath"
	"testing"
	"time"
)

// Different retained resolutions own different time ranges of the same input
// segment. Multiple tiers representing one observation must never be summed.
func TestTelemetryPlannerMixedRetentionAndNoDuplicateOwnership(t *testing.T) {
	_, e := telemetryFixture(t, "")
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Hour)
	at := now.Add(-6 * time.Hour)
	var raw []Point
	for i := 0; i < 36; i++ {
		b := telemetryBatch(time.Duration(i).String(), at.Add(time.Duration(i)*5*time.Minute), float64(i+1))
		if _, err := e.Accept(ctx, b, now); err != nil {
			t.Fatal(err)
		}
		raw = append(raw, b.Points...)
	}
	useLegacyOnlineTiers(t, e)
	summarizeAll(t, e)
	// Model real retention frontiers: older 15m, newer 5m. Keep the hour
	// tier overlapping both, so a 30m request must reject it as too coarse.
	boundary := at.Add(90 * time.Minute)
	if _, err := e.s.db.ExecContext(ctx, e.bind("DELETE FROM "+e.table("summary")+" WHERE resolution=? OR (resolution=? AND bucket<?) OR (resolution=? AND bucket>=?)"), int64(10*time.Second), int64(5*time.Minute), boundary.UnixNano(), int64(15*time.Minute), boundary.UnixNano()); err != nil {
		t.Fatal(err)
	}
	for _, interval := range []time.Duration{30 * time.Minute, time.Hour, 90 * time.Minute} {
		for _, aggregation := range []Aggregation{AggCount, AggSum, AggAvg, AggMin, AggMax, AggLast} {
			q := AggregateQuery{Query: Query{MetricName: "cpu", EntityID: "node", Start: at, End: at.Add(3*time.Hour - time.Nanosecond)}, Interval: interval, Aggregation: aggregation, PreserveSeries: true}
			want, err := AggregatePoints(raw, q)
			if err != nil {
				t.Fatal(err)
			}
			got, err := e.Chart(ctx, q, now)
			if err != nil {
				t.Fatalf("%s/%s: %v", interval, aggregation, err)
			}
			if len(got.Points) != len(want) {
				t.Fatalf("%s/%s: %d buckets, want %d", interval, aggregation, len(got.Points), len(want))
			}
			for i, p := range got.Points {
				tolerance := 1e-8
				if isPercentile(aggregation) {
					tolerance = 1
				}
				if p.Count != want[i].Count || !p.Bucket.Equal(want[i].Bucket) || math.Abs(p.Value-want[i].Value) > tolerance {
					t.Fatalf("%s/%s: got %+v, want %+v", interval, aggregation, p, want[i])
				}
			}
		}
	}
}

func TestTelemetryPlannerMigratedHotInputAndLateOnlineInputHaveDistinctOwnership(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := Open(ctx, SQLite(filepath.Join(dir, "metrics.db"), WithRetiredLegacyStorage(), WithSQLiteProfile(SQLiteProfileDurable)))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.CreateMetric(ctx, Definition{Name: "cpu", RetentionDays: 30}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Minute)
	at := now.Add(-10 * time.Minute)
	if err = s.WriteBatch(ctx, telemetryBatch("", at, 2, 8).Points); err != nil {
		t.Fatal(err)
	}
	cfg := TelemetryConfig{ArchiveDirectory: filepath.Join(dir, "archive")}
	if err = s.MigrateLegacy(ctx, cfg, nil); err != nil {
		t.Fatal(err)
	}
	if err = s.EnableTelemetry(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	e := s.Telemetry()
	if _, err = e.Accept(ctx, telemetryBatch("late", at.Add(time.Second), 20), now); err != nil {
		t.Fatal(err)
	}
	q := AggregateQuery{Query: Query{MetricName: "cpu", EntityID: "node", Start: at, End: at.Add(time.Minute - time.Nanosecond)}, Interval: time.Minute, Aggregation: AggSum, PreserveSeries: true}
	for pass := 0; pass < 2; pass++ {
		r, err := e.Chart(ctx, q, now)
		if err != nil || len(r.Points) != 1 || r.Points[0].Count != 3 || r.Points[0].Value != 30 || len(r.Gaps) > 0 {
			t.Fatalf("migration/hot/late pass %d: %+v %v", pass, r, err)
		}
		summarizeAll(t, e)
	}
}

func TestTelemetryPlannerMixedPingUsesProbeWeights(t *testing.T) {
	_, e := telemetryFixture(t, "")
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Hour)
	at := now.Add(-6 * time.Hour)
	for i, v := range []float64{-1, 10, 20, -1, 40, 50} {
		b := telemetryBatch(time.Duration(i).String(), at.Add(time.Duration(i)*5*time.Minute), v)
		b.Points[0].MetricName = sqliteMergedPingLatencyMetric
		if _, err := e.Accept(ctx, b, now); err != nil {
			t.Fatal(err)
		}
	}
	useLegacyOnlineTiers(t, e)
	summarizeAll(t, e)
	if _, err := e.s.db.ExecContext(ctx, "DELETE FROM "+e.table("summary")+" WHERE resolution=? OR (resolution=? AND bucket<?) OR (resolution=? AND bucket>=?)", int64(10*time.Second), int64(5*time.Minute), at.Add(15*time.Minute).UnixNano(), int64(15*time.Minute), at.Add(15*time.Minute).UnixNano()); err != nil {
		t.Fatal(err)
	}
	q := AggregateQuery{Query: Query{MetricName: sqliteMergedPingLatencyMetric, EntityID: "node", Start: at, End: at.Add(30*time.Minute - time.Nanosecond)}, Interval: 30 * time.Minute, Aggregation: AggAvg, PreserveSeries: true}
	r, err := e.Chart(ctx, q, now)
	if err != nil || len(r.Points) != 1 || r.Points[0].Count != 4 || r.Points[0].Value != 30 {
		t.Fatalf("latency weighted by failed probes: %+v %v", r, err)
	}
	q.MetricName = sqliteVirtualPingLossMetric
	r, err = e.Chart(ctx, q, now)
	if err != nil || len(r.Points) != 1 || math.Abs(r.Points[0].Value-1.0/3) > 1e-9 {
		t.Fatalf("loss averaged from bucket percentages: %+v %v", r, err)
	}
}

func TestTelemetryPlannerLegacyPartialPromotionRemainsAdditive(t *testing.T) {
	_, e := telemetryFixture(t, "")
	ctx := context.Background()
	at := time.Now().UTC().Add(-4 * time.Hour).Truncate(time.Hour)
	sid := telemetryMigrationKey("cpu\x00node\x00{}")
	var cells []telemetryMigrationCell
	for i, v := range []float64{2, 8, 20} {
		var st telemetryStats
		p := Point{MetricName: "cpu", Timestamp: at.Add(time.Duration(i) * 10 * time.Second), Value: v}
		st.add(p, 1, i)
		cells = append(cells, telemetryMigrationCell{series: sid, tags: "{}", bucket: p.Timestamp.UnixNano(), resolution: 10 * time.Second, stats: st})
	}
	if err := e.commitLegacyCells(ctx, "partial-promotion", "cpu", "node", cells); err != nil {
		t.Fatal(err)
	}
	// Only one source cell is promoted. Its coarse cell and the two retained
	// fine cells overlap in time but own different input observations.
	if n, err := e.PromoteLegacyChunk(ctx, at.Add(4*time.Hour), 1); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	q := AggregateQuery{Query: Query{MetricName: "cpu", EntityID: "node", Start: at, End: at.Add(time.Hour - time.Nanosecond)}, Interval: time.Hour, Aggregation: AggSum, PreserveSeries: true}
	r, err := e.Chart(ctx, q, at.Add(4*time.Hour))
	if err != nil || len(r.Points) != 1 || r.Points[0].Value != 30 || r.Points[0].Count != 3 || len(r.Gaps) != 0 {
		t.Fatalf("partial canonical promotion lost input: %+v, %v", r, err)
	}
}

func TestTelemetryPlannerUnrelatedLegacyDoesNotClampRecentSeries(t *testing.T) {
	_, e := telemetryFixture(t, "")
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Minute)
	at := now.Add(-time.Minute)
	if _, err := e.Accept(ctx, telemetryBatch("recent", at, 7), now); err != nil {
		t.Fatal(err)
	}
	summarizeAll(t, e)
	// These persisted global markers may describe a completely different
	// historical metric. Precision must come from the selected cells only.
	e.legacyUntil, e.legacyResolution = now.UnixNano(), time.Hour
	q := AggregateQuery{Query: Query{MetricName: "cpu", EntityID: "node", Start: at, End: at.Add(time.Minute - time.Nanosecond)}, Interval: 10 * time.Second, Aggregation: AggSum, PreserveSeries: true}
	got, err := e.Chart(ctx, q, now)
	if err != nil || len(got.Points) != 1 || got.Points[0].Value != 7 {
		t.Fatalf("unrelated legacy marker degraded recent series: %+v, %v", got, err)
	}
}
