package metric

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestTelemetryPolicyPresetGridOracle(t *testing.T) {
	windows := []struct {
		hours    int
		interval time.Duration
		count    int
	}{
		{1, 10 * time.Second, 360}, {6, time.Minute, 360}, {12, 2 * time.Minute, 360}, {24, 5 * time.Minute, 288},
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

func TestTelemetryPolicyPromotionSurvivesImmediateRawHandoff(t *testing.T) {
	_, e := telemetryFixture(t, "")
	ctx := context.Background()
	at := time.Now().UTC().Truncate(10 * time.Second)
	e.cfg.RawRetention = 24 * time.Hour
	if _, err := e.Accept(ctx, telemetryBatch("separate-hot", at, 3, 7), at); err != nil {
		t.Fatal(err)
	}
	summarizeAll(t, e)
	if _, err := e.SealDue(ctx, at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if delay, err := e.NextSummaryPromotion(ctx, at.Add(30*time.Minute)); err != nil || delay != 30*time.Minute+10*time.Second {
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
	if n, err := e.DeleteArchivedChunk(ctx, at.Add(time.Hour), 512); err != nil || n != 2 {
		t.Fatal("retired setting kept archived raw SQL", n, err)
	}
	q := Query{MetricName: "cpu", EntityID: "node", Start: at, End: at.Add(time.Minute)}
	if _, err := e.ReadHot(ctx, q, at.Add(23*time.Hour)); err == nil {
		t.Fatal("one-day safety rows enabled unbounded online raw reads")
	}
	if delay, err := e.NextSummaryPromotion(ctx, at.Add(30*time.Minute)); err != nil || delay != 30*time.Minute+10*time.Second {
		t.Fatal("raw handoff changed chart promotion deadline", delay, err)
	}
	chart, err := e.Chart(ctx, AggregateQuery{Query: q, Interval: time.Minute, Aggregation: AggAvg}, at.Add(30*time.Minute))
	if err != nil || len(chart.Points) != 1 || chart.Points[0].Count != 2 || chart.Points[0].Value != 5 {
		t.Fatal("chart changed after raw handoff", chart.Points, err)
	}
}
