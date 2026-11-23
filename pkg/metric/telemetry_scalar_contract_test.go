package metric

import (
	"context"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"
)

func scalarReferenceCount(s telemetryStats, a Aggregation) int {
	if a == AggLast {
		return 1
	}
	if a == aggPingLossAvg {
		return int(s.Attempts)
	}
	return int(s.N)
}

func seedScalarCell(t *testing.T, e *Telemetry, at time.Time, resolution time.Duration, values ...float64) {
	t.Helper()
	var st telemetryStats
	for i, v := range values {
		addScalarStats(&st, Point{MetricName: "cpu", Timestamp: at.Add(time.Duration(i) * time.Millisecond), Value: v}, 1, i)
	}
	c := telemetryMigrationCell{series: telemetryMigrationKey("cpu\x00node\x00{}"), tags: "{}", bucket: at.UnixNano(), resolution: resolution, stats: st, raw: st}
	if err := e.commitScalarLegacyCells(context.Background(), fmt.Sprint("test/", at, resolution), "cpu", "node", []telemetryMigrationCell{c}); err != nil {
		t.Fatal(err)
	}
}

func TestScalarEveryExpiryKeepsOnePopulation(t *testing.T) {
	for i, tier := range telemetrySummaryTiers[:len(telemetrySummaryTiers)-1] {
		t.Run(tier.resolution.String(), func(t *testing.T) {
			_, e := telemetryFixture(t, "")
			ctx := context.Background()
			at := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
			target := telemetrySummaryTiers[i+1].resolution
			seedScalarCell(t, e, at, tier.resolution, 1, 4, 9)
			deadline := at.Add(tier.keep + tier.resolution)
			for _, delta := range []time.Duration{-time.Nanosecond, 0, time.Nanosecond} {
				now := deadline.Add(delta)
				promoteTelemetryAll(t, e, now)
				var count int
				if err := e.s.db.QueryRow(e.bind("SELECT COUNT(*) FROM "+e.table("rollup")+" WHERE resolution=?"), int64(tier.resolution/time.Second)).Scan(&count); err != nil {
					t.Fatal(err)
				}
				if delta < 0 && count != 1 || delta >= 0 && count != 0 {
					t.Fatalf("source expiry %s: %d", delta, count)
				}
				q := scalarQuery(at, target, target, "cpu")
				q.Aggregation = AggSum
				r, err := e.Chart(ctx, q, now)
				if err != nil || len(r.Gaps) != 0 || len(r.Points) != 1 || r.Points[0].Count != 3 || r.Points[0].Value != 14 || telemetryCount(t, e, "rollup") != 1 {
					t.Fatal("lost or repeated population", r, err)
				}
			}
			for {
				n, err := e.PruneSummaries(ctx, "cpu", time.Hour, deadline.Add(2*time.Hour), 1)
				if err != nil {
					t.Fatal(err)
				}
				if n == 0 {
					break
				}
			}
			if telemetryCount(t, e, "rollup") != 0 || telemetryCount(t, e, "rollup_due") != 0 {
				t.Fatal("expired rows/due markers remain")
			}
		})
	}
}

func TestScalarFreshInputDoesNotGenerateAncestors(t *testing.T) {
	_, e := telemetryFixture(t, "")
	ctx := context.Background()
	at := time.Now().UTC().Truncate(10 * time.Second)
	if _, err := e.Accept(ctx, telemetryBatch("fresh", at, 3, 7, 11), at); err != nil {
		t.Fatal(err)
	}
	summarizeAll(t, e)
	var resolution int64
	if err := e.s.db.QueryRow("SELECT MAX(resolution) FROM " + e.table("rollup")).Scan(&resolution); err != nil || resolution != 10 || telemetryCount(t, e, "rollup") != 1 {
		t.Fatal(resolution, err)
	}
	promoteTelemetryAll(t, e, at.Add(time.Hour+telemetryTierGrace+10*time.Second))
	if err := e.s.db.QueryRow("SELECT MAX(resolution) FROM " + e.table("rollup")).Scan(&resolution); err != nil || resolution != 60 || telemetryCount(t, e, "rollup") != 1 {
		t.Fatal(resolution, err)
	}
}

func TestMinuteChartKeepsPopulationThroughFirstTierPromotion(t *testing.T) {
	_, e := telemetryFixture(t, "")
	ctx := context.Background()
	at := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Minute)
	batch := telemetryBatch("promotion-pair", at.Add(time.Minute), 0.1, 0.2, 0.3)
	for i := range batch.Points {
		batch.Points[i].Timestamp = at.Add(time.Minute + time.Duration(1+5*i)*time.Second)
	}
	if _, err := e.Accept(ctx, batch, time.Now()); err != nil {
		t.Fatal(err)
	}
	summarizeAll(t, e)

	start := at.Add(2 * time.Second)
	query := Query{MetricName: "cpu", EntityID: "node", Start: start, End: start.Add(time.Hour)}
	read := func(now time.Time) (Query, time.Duration, TelemetryQueryResult) {
		t.Helper()
		planned, interval, err := e.PlanChart(ctx, query, now, 60, 0)
		if err != nil {
			t.Fatal(err)
		}
		result, err := e.Chart(ctx, AggregateQuery{Query: planned, Interval: interval, Aggregation: AggAvg}, now)
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Points) != 1 || len(result.Gaps) != 0 {
			t.Fatalf("incomplete chart: %+v", result)
		}
		return planned, interval, result
	}
	promotionTime := at.Add(time.Hour + telemetryTierGrace + time.Minute + 10*time.Second)
	beforeWindow, beforeInterval, before := read(promotionTime.Add(-time.Second))
	promoteTelemetryAll(t, e, promotionTime)
	afterWindow, afterInterval, after := read(promotionTime.Add(time.Second))
	if beforeInterval != time.Minute || afterInterval != beforeInterval ||
		!beforeWindow.Start.Equal(afterWindow.Start) || !beforeWindow.End.Equal(afterWindow.End) {
		t.Fatalf("chart grid changed: before %+v/%v after %+v/%v", beforeWindow, beforeInterval, afterWindow, afterInterval)
	}
	if before.Resolution != 10*time.Second || after.Resolution != time.Minute {
		t.Fatalf("source resolution did not promote: %v -> %v", before.Resolution, after.Resolution)
	}
	a, b := before.Points[0], after.Points[0]
	if !a.Bucket.Equal(b.Bucket) || a.Count != 3 || b.Count != a.Count || math.Abs(a.Value-b.Value) > 1e-14 {
		t.Fatalf("population changed during promotion: %+v -> %+v", a, b)
	}
}

func TestScalarSharedQueryBudgetAndUnsupportedDistribution(t *testing.T) {
	_, e := telemetryFixture(t, "")
	ctx := context.Background()
	at := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Minute)
	if _, err := e.Accept(ctx, telemetryBatch("base", at, 1, 2, 3), time.Now()); err != nil {
		t.Fatal(err)
	}
	summarizeAll(t, e)
	if _, err := e.Accept(ctx, telemetryBatch("pending", at.Add(time.Second), 100, 101, 102), time.Now()); err != nil {
		t.Fatal(err)
	}
	q := scalarQuery(at, time.Minute, time.Minute, "cpu")
	r, err := e.ChartBatchBounded(ctx, []AggregateQuery{q}, time.Now(), 100, 32<<10)
	if err != nil || len(r) != 1 || len(r[0].Points) != 1 || r[0].Points[0].Count != 6 || r[0].Points[0].Value != 51.5 {
		t.Fatal(r, err)
	}
	if _, err = e.ChartBatchBounded(ctx, []AggregateQuery{q, q}, time.Now(), 1, 32<<10); !errors.Is(err, ErrTelemetryBackpressure) {
		t.Fatal("shared count budget", err)
	}
	if _, err = e.ChartBatchBounded(ctx, []AggregateQuery{q}, time.Now(), 100, 1); !errors.Is(err, ErrTelemetryBackpressure) {
		t.Fatal("shared byte budget", err)
	}
	for _, agg := range []Aggregation{AggP50, AggP95, AggP99, AggStdDev, AggFirst} {
		q.Aggregation = agg
		if _, err = e.Chart(ctx, q, time.Now()); !errors.Is(err, ErrInvalidArgument) {
			t.Fatal(agg, err)
		}
	}
}

func TestScalarCompletedMigrationDrainsLastShortRawSegment(t *testing.T) {
	s, e := telemetryFixture(t, "")
	ctx := context.Background()
	now := time.Now().UTC()
	at := now.Add(-time.Minute)
	if _, err := e.Accept(ctx, telemetryBatch("last-short", at, 1, 4, 9), now); err != nil {
		t.Fatal(err)
	}
	summarizeAll(t, e)
	for _, sql := range []string{"INSERT INTO " + e.table("progress") + "(task,value) VALUES ('legacy_migration_complete',1)", "INSERT INTO " + e.table("progress") + "(task,value) VALUES ('legacy_migration_now'," + fmt.Sprint(now.UnixNano()) + ")"} {
		if _, err := s.db.Exec(sql); err != nil {
			t.Fatal(err)
		}
	}
	cfg := e.cfg
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.MigrateLegacy(ctx, cfg, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.MigrateLegacy(ctx, cfg, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.EnableTelemetry(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	target := s.Telemetry()
	defer target.Close()
	var pending int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM " + target.table("hot") + " WHERE segment_id<>''").Scan(&pending); err != nil || pending != 0 {
		t.Fatal("completion stranded archive input", pending, err)
	}
	if pendingBytes, err := target.progress(ctx, s.db, "hot_bytes"); err != nil || pendingBytes != 0 {
		t.Fatal("completion stranded archive budget", pendingBytes, err)
	}
	if telemetryCount(t, target, "hot") != 3 {
		t.Fatal("migration discarded the live display window")
	}
	raw, err := target.ExportRawBatch(WithTelemetryEntities(ctx, []string{"node"}), []Query{{MetricName: "cpu", EntityID: "node", Start: at, End: now}})
	if err != nil || len(raw) != 1 || len(raw[0]) != 3 {
		t.Fatal(raw, err)
	}
}

func testScalarExternalBoundary(t *testing.T, e *Telemetry) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	at := now.Add(-3 * 24 * time.Hour).Truncate(time.Hour)
	e.cfg.MaxLateness = 7 * 24 * time.Hour
	b := TelemetryBatch{Source: "scalar-boundary", Identity: "coarse", Points: []Point{{MetricName: "cpu", EntityID: "scalar-boundary", Timestamp: at, Value: 2}, {MetricName: "cpu", EntityID: "scalar-boundary", Timestamp: at.Add(20 * time.Minute), Value: 8}}}
	if _, err := e.Accept(ctx, b, now); err != nil {
		t.Fatal(err)
	}
	summarizeAll(t, e)
	q := scalarQuery(at, time.Hour, 10*time.Minute, "cpu")
	q.EntityID = "scalar-boundary"
	r, err := e.Chart(ctx, q, now)
	if err != nil || len(r.Gaps) == 0 || len(r.Points) != 0 {
		t.Fatal("coarse bucket was split", r, err)
	}
	q.Interval = time.Hour
	r, err = e.Chart(ctx, q, now)
	if err != nil || len(r.Gaps) != 0 || len(r.Points) != 1 || r.Points[0].Count != 2 || r.Points[0].Value != 5 {
		t.Fatal(r, err)
	}
	testScalarRecentExternalBoundary(t, e)
}
