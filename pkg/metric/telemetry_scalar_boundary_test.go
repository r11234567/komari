package metric

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestScalarRecentBoundariesStayExactAcrossSummaryHandoffAndRestart(t *testing.T) {
	for _, name := range []string{"cpu", sqliteMergedPingLatencyMetric} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			s, e := telemetryFixture(t, dir)
			ctx := context.Background()
			at := time.Now().UTC().Add(-time.Minute).Truncate(10 * time.Second)
			now := at.Add(time.Minute)
			batch := func(id string, second int, value float64) {
				t.Helper()
				b := TelemetryBatch{Source: "node", Identity: id, Points: []Point{{MetricName: name, EntityID: "node", Timestamp: at.Add(time.Duration(second) * time.Second), Value: value}}}
				if _, err := e.Accept(ctx, b, now); err != nil {
					t.Fatal(err)
				}
			}
			// Both edges split one 10s scalar cell. The middle point is accepted
			// after the summary cursor, so it must appear exactly once as a tail.
			batch("first", 2, 2)
			batch("last", 7, 8)
			summarizeAll(t, e)
			middle := 4.0
			if name == sqliteMergedPingLatencyMetric {
				middle = -1 // a real lost probe, never a negative latency sample
			}
			batch("pending", 4, middle)
			check := func(phase string) {
				t.Helper()
				for _, bounds := range [][2]int{{0, 6}, {3, 9}, {3, 6}} {
					q := AggregateQuery{Query: Query{MetricName: name, EntityID: "node", Start: at.Add(time.Duration(bounds[0]) * time.Second), End: at.Add(time.Duration(bounds[1]) * time.Second)}, Interval: 10 * time.Second, Aggregation: AggCount, PreserveSeries: true}
					raw, err := e.ReadHot(ctx, q.Query, now)
					if err != nil {
						t.Fatal(err)
					}
					count, valid := len(raw), 0
					sum := 0.0
					for _, p := range raw {
						if name != sqliteMergedPingLatencyMetric || p.Value >= 0 {
							valid++
							sum += p.Value
						}
					}
					r, err := e.Chart(ctx, q, now)
					if err != nil || len(r.Gaps) != 0 {
						t.Fatalf("%s %v: %v %v", phase, bounds, r.Gaps, err)
					}
					got := 0
					for _, p := range r.Points {
						got += p.Count
						if p.Value != float64(p.Count) {
							t.Fatal("count value", p)
						}
					}
					if got != count {
						t.Fatalf("%s %v: got %d want %d", phase, bounds, got, count)
					}
					q.Aggregation = AggAvg
					avg, err := e.Chart(ctx, q, now)
					if err != nil || len(avg.Gaps) != 0 {
						t.Fatal("average coverage", avg.Gaps, err)
					}
					if valid == 0 {
						if len(avg.Points) != 0 {
							t.Fatal("lost probe became a latency", avg.Points)
						}
					} else if len(avg.Points) != 1 || avg.Points[0].Count != valid || avg.Points[0].Value != sum/float64(valid) {
						t.Fatal("average population changed", avg.Points, valid, sum)
					}
				}
			}
			check("summary plus pending tail")
			summarizeAll(t, e)
			check("fully summarized")
			lifecycleArchiveAll(t, e, now)
			check("detached live rows after handoff")
			if err := e.Close(); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s, e = telemetryFixture(t, dir)
			check("reopened")
			// Removing even an out-of-window member prevents claiming that the
			// remaining raw subset proves the scalar cell's complete population.
			if _, err := s.db.Exec("DELETE FROM "+e.table("hot")+" WHERE observed=?", at.Add(7*time.Second).UnixNano()); err != nil {
				t.Fatal(err)
			}
			q := AggregateQuery{Query: Query{MetricName: name, EntityID: "node", Start: at, End: at.Add(6 * time.Second)}, Interval: 10 * time.Second, Aggregation: AggCount, PreserveSeries: true}
			if r, err := e.Chart(ctx, q, now); err != nil || len(r.Gaps) == 0 || len(r.Points) != 0 {
				t.Fatal("incomplete raw population fabricated coverage", r, err)
			}
		})
	}
}

func TestScalarRecentBoundaryRefinementSharesHotReadBudget(t *testing.T) {
	_, e := telemetryFixture(t, "")
	ctx := context.Background()
	at := time.Now().UTC().Add(-time.Minute).Truncate(10 * time.Second)
	for batch := 0; batch < 44; batch++ {
		b := TelemetryBatch{Source: "node", Identity: fmt.Sprint(batch)}
		for i := 0; i < 250; i++ {
			b.Points = append(b.Points, Point{MetricName: "cpu", EntityID: "node", Timestamp: at.Add(time.Duration(batch*250+i) * time.Microsecond), Value: 1})
		}
		if _, err := e.Accept(ctx, b, at.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	summarizeAll(t, e)
	q := AggregateQuery{Query: Query{MetricName: "cpu", EntityID: "node", Start: at, End: at.Add(5 * time.Millisecond)}, Interval: 10 * time.Second, Aggregation: AggCount, PreserveSeries: true}
	if _, err := e.Chart(ctx, q, at.Add(time.Minute)); err != ErrTelemetryBackpressure {
		t.Fatal("boundary raw scan exceeded shared 10000-row bound", err)
	}
}

func TestScalarRecentBoundaryDoesNotReuseRawAcrossAdditiveTiers(t *testing.T) {
	s, e := telemetryFixture(t, "")
	ctx := context.Background()
	at := time.Now().UTC().Add(-time.Minute).Truncate(time.Minute)
	b := TelemetryBatch{Source: "node", Identity: "base", Points: []Point{{MetricName: "cpu", EntityID: "node", Timestamp: at.Add(2 * time.Second), Value: 2}, {MetricName: "cpu", EntityID: "node", Timestamp: at.Add(7 * time.Second), Value: 8}}}
	if _, err := e.Accept(ctx, b, at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	summarizeAll(t, e)
	// A migrated coarse contribution can share wall-clock time with a later
	// fine contribution. Equal counts do not give either cell ownership of
	// the other cell's raw rows.
	var key int64
	if err := s.db.QueryRow("SELECT series_id FROM " + e.table("rollup")).Scan(&key); err != nil {
		t.Fatal(err)
	}
	tx, err := e.begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var st telemetryStats
	for i, p := range b.Points {
		addScalarStats(&st, p, 0, i)
	}
	if err = e.putScalar(ctx, tx, scalarCell{key, 60, at.Unix(), st}); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	q := AggregateQuery{Query: Query{MetricName: "cpu", EntityID: "node", Start: at, End: at.Add(6 * time.Second)}, Interval: 10 * time.Second, Aggregation: AggCount, PreserveSeries: true}
	if r, err := e.Chart(ctx, q, at.Add(time.Minute)); err != nil || len(r.Gaps) == 0 || len(r.Points) != 0 {
		t.Fatal("raw counted for more than one additive owner", r, err)
	}
}

// Called by the actual PostgreSQL/MySQL/MariaDB fixture as well as SQLite.
func testScalarRecentExternalBoundary(t *testing.T, e *Telemetry) {
	t.Helper()
	ctx := context.Background()
	at := time.Now().UTC().Add(-time.Minute).Truncate(10 * time.Second)
	for i, second := range []int{2, 7, 4} {
		b := TelemetryBatch{Source: "scalar-recent", Identity: fmt.Sprint(i), Points: []Point{{MetricName: "cpu", EntityID: "scalar-recent", Timestamp: at.Add(time.Duration(second) * time.Second), Value: float64(second)}}}
		if _, err := e.Accept(ctx, b, at.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		if i == 1 {
			summarizeAll(t, e)
		}
	}
	q := AggregateQuery{Query: Query{MetricName: "cpu", EntityID: "scalar-recent", Start: at.Add(3 * time.Second), End: at.Add(6 * time.Second)}, Interval: 10 * time.Second, Aggregation: AggAvg, PreserveSeries: true}
	for phase := 0; phase < 2; phase++ {
		r, err := e.Chart(ctx, q, at.Add(time.Minute))
		if err != nil || len(r.Gaps) != 0 || len(r.Points) != 1 || r.Points[0].Count != 1 || r.Points[0].Value != 4 {
			t.Fatal("real SQL recent boundary", phase, r, err)
		}
		summarizeAll(t, e)
	}
}
