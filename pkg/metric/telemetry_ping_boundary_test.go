package metric

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"
)

func TestTelemetryPingWindowKeepsHotEdgesAndAdvertisesHistoricalGrid(t *testing.T) {
	_, e := telemetryFixture(t, "")
	now := time.Date(2026, 11, 11, 12, 34, 56, 123, time.UTC)
	start, end := now.Add(-time.Minute), now
	actualStart, actualEnd, err := e.PingWindow(start, end, 10*time.Second, now)
	if err != nil || !actualStart.Equal(start) || !actualEnd.Equal(end) {
		t.Fatalf("recent exact bounds changed: %v %v %v", actualStart, actualEnd, err)
	}
	for _, interval := range []time.Duration{10 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 30 * time.Minute, time.Hour, 2 * time.Hour, 7 * time.Hour} {
		start, end = now.Add(-48*time.Hour), now.Add(-time.Hour)
		actualStart, actualEnd, err = e.PingWindow(start, end, interval, now)
		if err != nil || actualStart.Before(start) || actualEnd.After(end) || !alignTime(actualStart, interval).Equal(actualStart) || !alignTime(actualEnd.Add(time.Nanosecond), interval).Equal(actualEnd.Add(time.Nanosecond)) {
			t.Fatalf("%s historical bounds: %v %v %v", interval, actualStart, actualEnd, err)
		}
	}
	start = now.Add(-30 * time.Minute).Truncate(10 * time.Second)
	end = start.Add(10*time.Second - time.Nanosecond)
	actualStart, actualEnd, err = e.PingWindow(start, end, 10*time.Second, now)
	if err != nil || !actualStart.Equal(start) || !actualEnd.Equal(end) {
		t.Fatalf("inclusive aligned bucket changed: %v %v %v", actualStart, actualEnd, err)
	}
}

func TestTelemetryPingAlignedWindowAfterArchiveAndRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, e := telemetryFixture(t, dir)
	now := time.Now().UTC().Truncate(time.Hour)
	at := now.Add(-2 * time.Hour)
	var raw []Point
	for offset := 0; offset < 3600; offset += 5 {
		value := float64(10 + offset%80)
		if offset%35 == 0 {
			value = -1
		}
		p := Point{MetricName: sqliteMergedPingLatencyMetric, EntityID: "node", Timestamp: at.Add(time.Duration(offset)*time.Second + 900*time.Millisecond), Value: value, Tags: map[string]string{"task_id": "1"}}
		raw = append(raw, p)
		if _, err := e.Accept(ctx, TelemetryBatch{Source: "node", Identity: fmt.Sprint(offset), Points: []Point{p}}, now); err != nil {
			t.Fatal(err)
		}
	}
	summarizeAll(t, e)
	if _, err := e.SealDue(ctx, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	promoteTelemetryAll(t, e, now)
	for {
		ok, err := e.ArchiveNext(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			break
		}
	}
	for {
		n, err := e.DeleteArchivedChunk(ctx, now.Add(time.Hour), 512)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
	}
	if n := telemetryCount(t, e, "hot"); n != 0 {
		t.Fatal("historical hot points survived archive handoff", n)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	var err error
	s, e = telemetryFixture(t, dir)
	// Closing the cold root ensures online statistics cannot quietly recover
	// precision by decompressing archived observations during the sweep.
	if err = e.root.Close(); err != nil {
		t.Fatal(err)
	}
	for offset := 0; offset < 20000; offset += 137 {
		requestedStart := at.Add(time.Duration(offset)*time.Millisecond + time.Minute)
		requestedEnd := at.Add(59*time.Minute + time.Duration(offset)*time.Millisecond)
		start, end, err := e.PingWindow(requestedStart, requestedEnd, 10*time.Second, now)
		if err != nil || start.Before(requestedStart) || end.After(requestedEnd) {
			t.Fatalf("expanded advertised bounds: %v %v %v", start, end, err)
		}
		q := AggregateQuery{Query: Query{MetricName: sqliteMergedPingLatencyMetric, EntityID: "node", Start: start, End: end}, Interval: 10 * time.Second, PreserveSeries: true}
		got, err := e.PingSummary(ctx, q, now)
		if err != nil || len(got.Avg) != 1 || len(got.Loss) != 1 {
			t.Fatalf("rolling boundary %v failed after cold handoff: %+v %v", requestedStart, got, err)
		}
		var reference telemetryStats
		var validValues []float64
		for ordinal, p := range raw {
			if !p.Timestamp.Before(start) && !p.Timestamp.After(end) {
				reference.add(p, int64(ordinal+1), 0)
				if p.Value >= 0 {
					validValues = append(validValues, p.Value)
				}
			}
		}
		for _, view := range []struct {
			points []AggregatePoint
			agg    Aggregation
		}{{got.Avg, AggAvg}, {got.Min, AggMin}, {got.Max, AggMax}, {got.Last, AggLast}, {got.Loss, aggPingLossAvg}} {
			if view.agg == AggLast && reference.Last < 0 {
				if len(view.points) != 0 {
					t.Fatal("failed latest probe became valid latency", view.points)
				}
				continue
			}
			want, err := reference.value(view.agg)
			tolerance := 1e-8
			if isPercentile(view.agg) {
				quantile, _ := parsePercentile(view.agg)
				want = exactQuantile(validValues, quantile)
				tolerance = 1
			}
			if err != nil || len(view.points) != 1 || view.points[0].Count != scalarReferenceCount(reference, view.agg) || math.Abs(view.points[0].Value-want) > tolerance {
				t.Fatalf("%s population mismatch at %v: %+v want count=%d value=%v error=%v", view.agg, start, view.points, reference.Attempts, want, err)
			}
		}
		if offset == 0 {
			for attempt := 0; attempt < 25; attempt++ {
				repeated, err := e.PingSummary(ctx, q, now)
				if err != nil || repeated.Avg[0].Value != got.Avg[0].Value || repeated.Loss[0].Value != got.Loss[0].Value {
					t.Fatalf("unchanged SQL snapshot changed percentiles: P50 %v/%v P99 %v/%v error=%v", got.P50, repeated.P50, got.P99, repeated.P99, err)
				}
			}
		}
	}
}
