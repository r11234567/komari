package metric

import (
	"context"
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
