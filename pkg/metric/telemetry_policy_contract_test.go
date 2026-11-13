package metric

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestTelemetryPolicyDoesNotPregenerateAncestors(t *testing.T) {
	_, e := telemetryFixture(t, "")
	ctx := context.Background()
	at := time.Date(2025, 4, 1, 0, 0, 0, 0, time.UTC)
	if _, err := e.Accept(ctx, telemetryBatch("fresh-policy", at, 3, 7, 11), at); err != nil {
		t.Fatal(err)
	}
	summarizeAll(t, e)
	if _, err := e.SealDue(ctx, at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if n, err := e.PromoteSummariesChunk(ctx, at.Add(30*time.Minute), 512); err != nil || n != 0 {
		t.Fatal("fresh data was promoted", n, err)
	}
	if n, err := e.PromoteSummariesChunk(ctx, at.Add(time.Hour), 512); err != nil || n != 1 {
		t.Fatal("due minute parent missing", n, err)
	}
	var largest int64
	if err := e.s.db.QueryRow("SELECT MAX(resolution) FROM " + e.table("summary")).Scan(&largest); err != nil || largest != int64(time.Minute) {
		t.Fatal("ancestors were generated before expiry", largest, err)
	}
}

func TestTelemetryPolicyEveryExpiryPreservesOnlyPopulation(t *testing.T) {
	for i, tier := range telemetrySummaryTiers[:len(telemetrySummaryTiers)-1] {
		t.Run(tier.resolution.String(), func(t *testing.T) {
			_, e := telemetryFixture(t, "")
			ctx := context.Background()
			at := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
			target := telemetrySummaryTiers[i+1].resolution
			if _, err := e.Accept(ctx, telemetryBatch("expiry", at, 1, 4, 9), at); err != nil {
				t.Fatal(err)
			}
			summarizeAll(t, e)
			if _, err := e.SealDue(ctx, at.Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			deadline := at.Add(tier.keep + tier.resolution)
			for _, delta := range []time.Duration{-time.Nanosecond, 0, time.Nanosecond} {
				now := deadline.Add(delta)
				promoteTelemetryAll(t, e, now)
				for {
					n, err := e.PruneSummaries(ctx, "cpu", 800*24*time.Hour, now, 1)
					if err != nil {
						t.Fatal(err)
					}
					if n == 0 {
						break
					}
				}
				var n int
				if err := e.s.db.QueryRow("SELECT COUNT(*) FROM "+e.table("summary")+" WHERE resolution=?", int64(tier.resolution)).Scan(&n); err != nil {
					t.Fatal(err)
				}
				if delta < 0 && n == 0 || delta >= 0 && n != 0 {
					t.Fatalf("expiry %s delta %s cells %d", tier.resolution, delta, n)
				}
				q := AggregateQuery{Query: Query{MetricName: "cpu", EntityID: "node", Start: at, End: at.Add(target - time.Nanosecond)}, Interval: target, Aggregation: AggSum, PreserveSeries: true}
				r, err := e.Chart(ctx, q, now)
				if err != nil || len(r.Gaps) != 0 || len(r.Points) != 1 || r.Points[0].Count != 3 || r.Points[0].Value != 14 {
					t.Fatalf("lost/double population: %+v %v", r, err)
				}
			}
		})
	}
}

func TestTelemetryPolicyPresetGridOracle(t *testing.T) {
	windows := []struct {
		hours    int
		interval time.Duration
		count    int
	}{
		{1, 10 * time.Second, 360}, {6, time.Minute, 360}, {12, 2 * time.Minute, 360}, {24, 4 * time.Minute, 360},
		{168, 30 * time.Minute, 336}, {360, time.Hour, 360}, {720, 2 * time.Hour, 360}, {1080, 2 * time.Hour, 540},
		{1440, 4 * time.Hour, 360}, {1800, 6 * time.Hour, 300}, {2160, 6 * time.Hour, 360}, {2520, 6 * time.Hour, 420},
		{2880, 6 * time.Hour, 480}, {4320, 12 * time.Hour, 360}, {8760, 24 * time.Hour, 365},
	}
	for _, w := range windows {
		span := time.Duration(w.hours) * time.Hour
		if got := TelemetryChartInterval(span, 600); got != w.interval || int(span/got) != w.count {
			t.Fatalf("%dh: %s/%d want %s/%d", w.hours, got, span/got, w.interval, w.count)
		}
	}
	for d := 15; d <= 3650; d += 15 {
		span := time.Duration(d) * 24 * time.Hour
		for _, cap := range []int{50, 100, 299, 300, 360, 599, 600} {
			i := TelemetryChartInterval(span, cap)
			n := (span + i - 1) / i
			if n > time.Duration(cap) || cap == 600 && n < 300 {
				t.Fatal(fmt.Sprint(d, " days / cap ", cap, " -> ", n, " cells"))
			}
		}
	}
}

func TestTelemetryPolicyPromotionDueAndHotRetentionAreIndependent(t *testing.T) {
	_, e := telemetryFixture(t, "")
	ctx := context.Background()
	at := time.Date(2025, 4, 1, 0, 0, 0, 0, time.UTC)
	e.cfg.RawRetention = 24 * time.Hour
	if _, err := e.Accept(ctx, telemetryBatch("separate-hot", at, 3, 7), at); err != nil {
		t.Fatal(err)
	}
	summarizeAll(t, e)
	if _, err := e.SealDue(ctx, at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if delay, err := e.NextSummaryPromotion(ctx, at.Add(30*time.Minute)); err != nil || delay != 23*time.Minute+30*time.Second {
		t.Fatal("wrong indexed promotion deadline", delay, err)
	}
	for {
		ok, err := e.ArchiveNext(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			break
		}
	}
	if n, err := e.DeleteArchivedChunk(ctx, at.Add(23*time.Hour), 512); err != nil || n != 0 {
		t.Fatal("hot safety retention lost", n, err)
	}
	q := Query{MetricName: "cpu", EntityID: "node", Start: at, End: at.Add(time.Minute)}
	if _, err := e.ReadHot(ctx, q, at.Add(23*time.Hour)); err == nil {
		t.Fatal("one-day safety rows enabled unbounded online raw reads")
	}
	if n, err := e.DeleteArchivedChunk(ctx, at.Add(24*time.Hour), 512); err != nil || n != 2 {
		t.Fatal("hot rows did not retire at deadline", n, err)
	}
}

func TestTelemetryPolicyUpgradesPartiallyPromotedFifteenMinuteOwner(t *testing.T) {
	_, e := telemetryFixture(t, "")
	ctx := context.Background()
	at := time.Date(2025, 4, 1, 0, 0, 0, 0, time.UTC)
	var raw []Point
	for i := 0; i < 60; i++ {
		raw = append(raw, Point{MetricName: "cpu", EntityID: "node", Timestamp: at.Add(time.Duration(i) * time.Minute), Value: float64(i + 1)})
	}
	if _, err := e.Accept(ctx, TelemetryBatch{Source: "node", Identity: "partial-old-policy", Points: raw}, at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	useLegacyOnlineTiers(t, e)
	summarizeAll(t, e)
	if _, err := e.SealDue(ctx, at.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"UPDATE " + e.table("segments") + " SET summary_version=2",
		fmt.Sprintf("DELETE FROM %s WHERE resolution=%d OR resolution=%d", e.table("summary"), 10*time.Second, time.Hour),
		fmt.Sprintf("DELETE FROM %s WHERE resolution=%d AND bucket>=%d", e.table("summary"), 15*time.Minute, at.Add(30*time.Minute).UnixNano()),
		fmt.Sprintf("UPDATE %s SET promoted_through=1 WHERE resolution=%d AND bucket<%d", e.table("summary"), 5*time.Minute, at.Add(30*time.Minute).UnixNano()),
	} {
		if _, err := e.s.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	now := at.Add(3 * 24 * time.Hour)
	promoteTelemetryAll(t, e, now)
	for {
		n, err := e.PruneSummaries(ctx, "cpu", 30*24*time.Hour, now, 1)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
	}
	q := AggregateQuery{Query: Query{MetricName: "cpu", EntityID: "node", Start: at, End: at.Add(time.Hour - time.Nanosecond)}, Interval: 30 * time.Minute, Aggregation: AggSum, PreserveSeries: true}
	want, err := AggregatePoints(raw, q)
	if err != nil {
		t.Fatal(err)
	}
	r, err := e.Chart(ctx, q, now)
	if err != nil || len(r.Gaps) > 0 || len(r.Points) != 2 {
		t.Fatal("partial old handoff lost coverage", r, err)
	}
	for i, p := range r.Points {
		if p.Count != want[i].Count || p.Value != want[i].Value {
			t.Fatal("partial old handoff lost/doubled input", p, want[i])
		}
	}
}
