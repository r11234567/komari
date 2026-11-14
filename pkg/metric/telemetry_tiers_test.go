package metric

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// Fixtures that explicitly model the old 10s/5m/15m/1h online layout must
// exercise that producer, not fabricate tiers absent from the new layout.
func useLegacyOnlineTiers(t *testing.T, e *Telemetry) {
	t.Helper()
	if _, err := e.s.db.Exec("UPDATE " + e.table("segments") + " SET summary_version=1 WHERE state<>'legacy'"); err != nil {
		t.Fatal(err)
	}
}

func TestTelemetryTiersManySeriesAvoidReportTimeWriteAmplification(t *testing.T) {
	_, e := telemetryFixture(t, "")
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Hour)
	at := now.Add(-6 * time.Hour)
	for part := 0; part < 4; part++ {
		b := TelemetryBatch{Source: "node", Identity: fmt.Sprintf("cardinality/%d", part)}
		for series := part * 32; series < (part+1)*32; series++ {
			for i := 0; i < 6; i++ {
				b.Points = append(b.Points, Point{MetricName: "cpu", EntityID: "node", Tags: map[string]string{"device": fmt.Sprint(series)}, Timestamp: at.Add(time.Duration(i) * 10 * time.Second), Value: float64(series)})
			}
		}
		if _, err := e.Accept(ctx, b, now); err != nil {
			t.Fatal(err)
		}
	}
	started := time.Now()
	summarizeAll(t, e)
	baseTime := time.Since(started)
	if n := telemetryCount(t, e, "rollup"); n != 128 {
		t.Fatal("report fanout wrote redundant tiers", n)
	}
	if _, err := e.SealDue(ctx, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	started = time.Now()
	promoteTelemetryAll(t, e, now)
	t.Logf("128 series / 768 points: finest summary %s, one-time promotion %s", baseTime, time.Since(started))
	if n := telemetryCount(t, e, "rollup"); n != 128 {
		t.Fatal("unbounded duplicate parent cells", n)
	}
	r, err := e.Chart(ctx, AggregateQuery{Query: Query{MetricName: "cpu", EntityID: "node", Start: at, End: at.Add(time.Minute - time.Nanosecond)}, Interval: time.Minute, Aggregation: AggAvg, PreserveSeries: true}, now)
	if err != nil || len(r.Points) != 128 || len(r.Gaps) > 0 {
		t.Fatal("cardinality population", len(r.Points), err)
	}
	for _, p := range r.Points {
		if p.Count != 6 {
			t.Fatal(p)
		}
	}
}

func TestTelemetryTiersSixHourChartRetains360MinuteBuckets(t *testing.T) {
	_, e := telemetryFixture(t, "")
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Hour)
	at := now.Add(-6 * time.Hour)
	for part := 0; part < 2; part++ {
		b := TelemetryBatch{Source: "node", Identity: fmt.Sprintf("six-hours/%d", part)}
		for i := part * 180; i < (part+1)*180; i++ {
			b.Points = append(b.Points, Point{MetricName: "cpu", EntityID: "node", Timestamp: at.Add(time.Duration(i) * time.Minute), Value: float64(i)})
		}
		if _, err := e.Accept(ctx, b, now); err != nil {
			t.Fatal(err)
		}
	}
	summarizeAll(t, e)
	if _, err := e.SealDue(ctx, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	promoteTelemetryAll(t, e, now)
	for {
		n, err := e.PruneSummaries(ctx, "cpu", 30*24*time.Hour, now, 512)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
	}
	r, err := e.Chart(ctx, AggregateQuery{Query: Query{MetricName: "cpu", EntityID: "node", Start: at, End: now.Add(-time.Nanosecond)}, Interval: time.Minute, Aggregation: AggAvg, PreserveSeries: true}, now)
	if err != nil || len(r.Points) != 360 || len(r.Gaps) > 0 {
		t.Fatalf("six-hour minute coverage: got %d buckets, %d gaps; want 360 exact buckets: %v", len(r.Points), len(r.Gaps), err)
	}
}

func promoteTelemetryAll(t *testing.T, e *Telemetry, now time.Time) {
	t.Helper()
	for {
		n, err := e.PromoteSummariesChunk(context.Background(), now, 128)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			return
		}
	}
}

// Exercise the production ingest -> seal -> promotion -> pruning -> SQL chart
// path at the actual expiry boundaries. Expiring a child must neither remove
// the only population nor count it twice through a retained ancestor.
