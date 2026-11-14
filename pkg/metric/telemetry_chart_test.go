package metric

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"
)

func TestTelemetryChartBatchBudgetAcrossQueriesAndSources(t *testing.T) {
	_, e := telemetryFixture(t, "")
	ctx := WithTelemetryEntities(context.Background(), []string{"node"})
	at := time.Now().UTC().Truncate(time.Minute)
	if _, err := e.Accept(ctx, telemetryBatch("bounded", at, 1, 2, 3), at); err != nil {
		t.Fatal(err)
	}
	q := AggregateQuery{Query: Query{MetricName: "cpu", EntityID: "node", Start: at, End: at.Add(time.Minute)}, Interval: 10 * time.Second, Aggregation: AggAvg, PreserveSeries: true}
	for _, summarized := range []bool{false, true} {
		if summarized {
			summarizeAll(t, e)
		}
		if _, err := e.ChartBatchBounded(ctx, []AggregateQuery{q, q}, at, 1, 1<<20); !errors.Is(err, ErrTelemetryBackpressure) {
			t.Fatalf("unbounded shared result, summarized=%v: %v", summarized, err)
		}
		if _, err := e.ChartBatchBounded(ctx, []AggregateQuery{q}, at, 100, 1); !errors.Is(err, ErrTelemetryBackpressure) {
			t.Fatalf("unbounded input allocation: %v", err)
		}
		if got, err := e.ChartBatchBounded(ctx, []AggregateQuery{q}, at, 100, 1<<20); err != nil || len(got) != 1 || len(got[0].Points) == 0 {
			t.Fatalf("valid bounded chart: %+v %v", got, err)
		}
	}
}

func TestTelemetryChartExactBoundariesAndPendingTail(t *testing.T) {
	_, e := telemetryFixture(t, "")
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Minute)
	start := now.Add(-time.Minute)
	var points []Point
	for i, v := range []float64{1000, 1, 4, 7, 13, 20, 29, 1000} {
		at := start.Add(time.Duration(i*7-1) * time.Second)
		b := telemetryBatch(at.String(), at, v)
		if _, err := e.Accept(ctx, b, now); err != nil {
			t.Fatal(err)
		}
		points = append(points, b.Points...)
	}
	q := AggregateQuery{Query: Query{MetricName: "cpu", EntityID: "node", Start: start, End: start.Add(50*time.Second - time.Nanosecond)}, Interval: 10 * time.Second, Aggregation: AggAvg, PreserveSeries: true}
	var selected []Point
	for _, p := range points {
		if !p.Timestamp.Before(q.Start) && !p.Timestamp.After(q.End) {
			selected = append(selected, p)
		}
	}
	for _, a := range []Aggregation{AggCount, AggSum, AggAvg, AggLast, AggMin, AggMax} {
		q.Aggregation = a
		want, err := AggregatePoints(selected, q)
		if err != nil {
			t.Fatal(err)
		}
		for pass := 0; pass < 3; pass++ {
			got, err := e.Chart(ctx, q, now)
			if err != nil {
				t.Fatalf("%s pass%d: %v", a, pass, err)
			}
			if len(got.Points) != len(want) {
				t.Fatalf("%s buckets %+v want %+v", a, got.Points, want)
			}
			for i, p := range got.Points {
				if !p.Bucket.Equal(want[i].Bucket) || math.Abs(p.Value-want[i].Value) > 1e-8 {
					t.Fatalf("%s got %+v want %+v", a, p, want[i])
				}
			}
			if got.BoundaryAdjusted {
				t.Fatal("recent boundaries must be exact")
			}
			if pass == 0 {
				if _, err = e.SummarizeNext(ctx); err != nil {
					t.Fatal(err)
				}
			} else {
				summarizeAll(t, e)
			}
		}
	}
}

func TestTelemetryChartScopeAndColdCoverage(t *testing.T) {
	_, e := telemetryFixture(t, "")
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Hour)
	at := now.Add(-3 * time.Hour)
	for _, source := range []string{"node", "private-node"} {
		b := telemetryBatch(source, at, 2, 8)
		b.Source = source
		for i := range b.Points {
			b.Points[i].EntityID = source
		}
		if _, err := e.Accept(ctx, b, now); err != nil {
			t.Fatal(err)
		}
	}
	q := AggregateQuery{Query: Query{MetricName: "cpu", Start: at.Add(time.Second), End: at.Add(15 * time.Minute)}, Interval: 5 * time.Minute, Aggregation: AggCount, PreserveSeries: true}
	q.Start = at
	if r, err := e.Chart(ctx, q, now); err != nil || len(r.Points) != 2 {
		t.Fatalf("pending input disappeared: %+v %v", r, err)
	}
	summarizeAll(t, e)
	q.Start = at
	got, err := e.Chart(WithTelemetryEntities(ctx, []string{"node"}), q, now)
	if err != nil || len(got.Points) != 1 || got.Points[0].EntityID != "node" || got.Points[0].Value != 2 {
		t.Fatalf("scoped %+v %v", got, err)
	}
	got, err = e.Chart(WithTelemetryEntities(ctx, nil), q, now)
	if err != nil || len(got.Points) != 0 {
		t.Fatalf("empty scope %+v %v", got, err)
	}
	q.Start = at.Add(time.Second)
	got, err = e.Chart(ctx, q, now)
	if err != nil || got.BoundaryAdjusted || len(got.Points) != 0 {
		t.Fatalf("old boundary %+v %v", got, err)
	}
	// Known first/last observation times prove the previous window is empty.
	// Cutting through actual retained observations must instead disclose a gap.
	q.Start = at.Add(time.Nanosecond)
	got, err = e.Chart(ctx, q, now)
	if err != nil || !got.BoundaryAdjusted || len(got.Gaps) == 0 || len(got.Points) != 0 {
		t.Fatalf("unknown partial source was published as a complete value: %+v %v", got, err)
	}
	if err = e.root.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = e.Chart(ctx, q, now); err != nil {
		t.Fatalf("chart opened cold storage: %v", err)
	}
}
