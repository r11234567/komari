package metric

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"testing"
	"time"
)

func projectionLegacyCell(at time.Time, key, task string, resolution time.Duration) telemetryMigrationCell {
	var st telemetryStats
	for i, value := range []float64{10, -1, 30} {
		addScalarStats(&st, Point{MetricName: sqliteMergedPingLatencyMetric, Timestamp: at.Add(time.Duration(i) * time.Second), Value: value}, 1, i)
	}
	return telemetryMigrationCell{series: telemetryMigrationKey(key), tags: fmt.Sprintf(`{"task_id":%q}`, task), bucket: at.UnixNano(), resolution: resolution, stats: st}
}

func TestTelemetryDirectPingPlansMixedPrecisionInOneSnapshot(t *testing.T) {
	s, e := telemetryFixture(t, "")
	s.telemetry.Store(e)
	t.Cleanup(func() { s.telemetry.Store(nil) })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	now := time.Now().UTC().Truncate(time.Minute).Add(55 * time.Second)
	at := now.Add(-4 * time.Minute).Truncate(time.Minute)
	cells := []telemetryMigrationCell{
		projectionLegacyCell(at, "legacy/node", "1", time.Minute),
		projectionLegacyCell(at.Add(time.Minute), "legacy/node", "1", time.Minute),
	}
	if err := e.commitScalarLegacyCells(ctx, "mixed-import", sqliteMergedPingLatencyMetric, "node", cells); err != nil {
		t.Fatal(err)
	}
	var points []Point
	for i, value := range []float64{10, -1, 30} {
		points = append(points, Point{MetricName: sqliteMergedPingLatencyMetric, EntityID: "node", Tags: map[string]string{"task_id": "1"}, Timestamp: at.Add(2*time.Minute + 10*time.Second + time.Duration(i)*time.Second), Value: value})
	}
	if _, err := e.Accept(ctx, TelemetryBatch{Source: "node", Identity: "mixed-live", Points: points}, now); err != nil {
		t.Fatal(err)
	}
	summarizeAll(t, e)
	if _, err := s.db.Exec("DELETE FROM " + e.table("hot")); err != nil {
		t.Fatal(err)
	}
	// Planning must use the statistics transaction, including on a one-connection
	// reader. A second database read would wait forever for its own snapshot.
	s.reader().SetMaxOpenConns(1)
	q := AggregateQuery{Query: Query{MetricName: sqliteMergedPingLatencyMetric, EntityID: "node", Tags: map[string]string{"task_id": "1"}, Start: at.Add(time.Second), End: at.Add(2*time.Minute + 29*time.Second)}, Interval: 10 * time.Second}
	wantStart := at.Add(time.Minute)
	check := func(value SeriesSummary) {
		t.Helper()
		if value.Interval != time.Minute || !value.CoverageStart.Equal(wantStart) || !value.CoverageEnd.Equal(q.End) {
			t.Fatalf("actual precision/window: %+v", value)
		}
		if len(value.Loss) != 1 || value.Loss[0].Count != 6 || math.Abs(value.Loss[0].Value-1.0/3) > 1e-12 || len(value.Avg) != 1 || value.Avg[0].Count != 4 || value.Avg[0].Value != 20 {
			t.Fatalf("wrong retained population: %+v", value)
		}
	}
	value, err := s.PingSeriesSummary(ctx, q, now)
	if err != nil {
		t.Fatal(err)
	}
	check(value)
	for _, ids := range [][]string{{"node", "empty"}, {"empty", "node"}, {"", "node", "node", " ", "empty"}} {
		batch, err := s.PingSummariesForEntities(ctx, q, ids, now)
		if err != nil {
			t.Fatal(err)
		}
		if len(batch.ByEntity) != 2 || !batch.CoverageStart.Equal(wantStart) || !batch.CoverageEnd.Equal(q.End) || batch.Interval != time.Minute {
			t.Fatalf("batch metadata: %+v", batch)
		}
		check(batch.ByEntity["node"])
		empty := batch.ByEntity["empty"]
		if !empty.CoverageStart.Equal(batch.CoverageStart) || !empty.CoverageEnd.Equal(batch.CoverageEnd) || empty.Interval != batch.Interval || len(empty.Loss) != 0 {
			t.Fatalf("empty node changed the population: %+v", empty)
		}
	}
	q.Start, q.End = at.Add(time.Second), at.Add(19*time.Second)
	if _, err := s.PingSeriesSummary(ctx, q, now); !errors.Is(err, ErrTelemetryCoverage) {
		t.Fatalf("uncuttable source must remain unavailable: %v", err)
	}
}

func TestTelemetryPingTaskScopeControlsPlanningAndStatistics(t *testing.T) {
	s, e := telemetryFixture(t, "")
	s.telemetry.Store(e)
	t.Cleanup(func() { s.telemetry.Store(nil) })
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Minute).Add(55 * time.Second)
	at := now.Add(-3 * time.Minute).Truncate(time.Minute)
	var cells []telemetryMigrationCell
	for _, offset := range []time.Duration{10 * time.Second, 20 * time.Second} {
		cells = append(cells, projectionLegacyCell(at.Add(offset), "good/task", "1", 10*time.Second))
	}
	if err := e.commitScalarLegacyCells(ctx, "good-import", sqliteMergedPingLatencyMetric, "node", cells); err != nil {
		t.Fatal(err)
	}
	bad := projectionLegacyCell(at, "bad/task", "2", time.Minute)
	bad.stats.LastNano = at.Add(59 * time.Second).UnixNano()
	if err := e.commitScalarLegacyCells(ctx, "bad-import", sqliteMergedPingLatencyMetric, "node", []telemetryMigrationCell{bad}); err != nil {
		t.Fatal(err)
	}
	q := AggregateQuery{Query: Query{MetricName: sqliteMergedPingLatencyMetric, EntityID: "node", Start: at.Add(11 * time.Second), End: at.Add(29 * time.Second), Tags: map[string]string{"task_id": "1"}}, Interval: 10 * time.Second}
	batch, err := s.PingSummariesForEntities(ctx, q, []string{"node", "empty"}, now)
	if err != nil {
		t.Fatal(err)
	}
	value := batch.ByEntity["node"]
	if batch.Interval != 10*time.Second || !batch.CoverageStart.Equal(at.Add(20*time.Second)) || !batch.CoverageEnd.Equal(q.End) || len(value.Loss) != 1 || value.Loss[0].Count != 3 || value.Loss[0].Tags["task_id"] != "1" || value.Avg[0].Value != 20 {
		t.Fatalf("unrequested task affected result: %+v", batch)
	}
	// Prove this fixture actually exposes the coarse, unanswerable other task.
	q.Tags = nil
	if _, err = s.PingSummariesForEntities(ctx, q, []string{"node"}, now); !errors.Is(err, ErrTelemetryCoverage) {
		t.Fatalf("control did not expose coarse task: %v", err)
	}
	// Empty authorization must exclude that same unanswerable hidden series.
	q.EntityID = ""
	query, interval, err := e.PlanChart(WithTelemetryEntities(ctx, nil), q.Query, now, 600, 30)
	if err != nil || interval != 10*time.Second || !query.Start.Equal(q.Start) || !query.End.Equal(q.End) {
		t.Fatalf("empty scope saw private cells: %+v %s %v", query, interval, err)
	}
	r, err := e.Chart(WithTelemetryEntities(ctx, nil), AggregateQuery{Query: query, Interval: interval, Aggregation: AggCount}, now)
	if err != nil || len(r.Points) != 0 || len(r.Gaps) != 0 {
		t.Fatalf("empty scope read hidden data: %+v %v", r, err)
	}
}

func TestLegacyPingBatchMetadataAndEmptyEntities(t *testing.T) {
	s, _ := telemetryFixture(t, "")
	ctx := context.Background()
	now := time.Now().UTC()
	at := now.Add(-2 * time.Minute).Truncate(time.Minute)
	if err := s.CreateMetric(ctx, Definition{Name: sqliteMergedPingLatencyMetric, RetentionDays: 30}); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteBatch(ctx, []Point{{MetricName: sqliteMergedPingLatencyMetric, EntityID: "node", Timestamp: at, Value: 10, Tags: map[string]string{"task_id": "1"}}}); err != nil {
		t.Fatal(err)
	}
	q := AggregateQuery{Query: Query{MetricName: sqliteMergedPingLatencyMetric, Start: at.Add(-time.Second), End: now}, Interval: 10 * time.Second, PreserveSeries: true}
	var prior EntityPingSummaries
	for i, ids := range [][]string{{"node", "empty"}, {"empty", "node"}, {"", "node", "node", " ", "empty"}} {
		batch, err := s.PingSummariesForEntities(ctx, q, ids, now)
		if err != nil {
			t.Fatal(err)
		}
		if !batch.CoverageStart.Equal(q.Start) || !batch.CoverageEnd.Equal(q.End) || batch.Interval != q.Interval || len(batch.ByEntity) != 2 {
			t.Fatalf("legacy batch window: %+v", batch)
		}
		for id, value := range batch.ByEntity {
			if !value.CoverageStart.Equal(q.Start) || !value.CoverageEnd.Equal(q.End) || value.Interval != q.Interval {
				t.Fatalf("legacy %s window: %+v", id, value)
			}
		}
		if len(batch.ByEntity["node"].Avg) != 1 || len(batch.ByEntity["empty"].Avg) != 0 {
			t.Fatal("wrong entity distribution", batch)
		}
		if i > 0 && !reflect.DeepEqual(prior, batch) {
			t.Fatal("order/duplicates changed results", prior, batch)
		}
		prior = batch
	}
	q.EntityID = "node"
	summary, err := s.PingSeriesSummary(ctx, q, now)
	if err != nil || !summary.CoverageStart.Equal(q.Start) || !summary.CoverageEnd.Equal(q.End) || summary.Interval != q.Interval {
		t.Fatalf("direct legacy metadata: %+v %v", summary, err)
	}
	batch, err := s.PingSummariesForEntities(ctx, q, []string{"", " "}, now)
	if err != nil || len(batch.ByEntity) != 0 || !batch.CoverageStart.Equal(q.Start) || !batch.CoverageEnd.Equal(q.End) {
		t.Fatalf("empty batch: %+v %v", batch, err)
	}
}
