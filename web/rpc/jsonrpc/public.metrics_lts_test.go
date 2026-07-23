package jsonrpc

import (
	"testing"
	"time"

	"github.com/komari-monitor/komari/database/history"
)

func TestLTSMetricRangeAcceptsFractionalHours(t *testing.T) {
	start, end, err := ltsMetricRange(ltsMetricQueryParams{Hours: 10.0 / 60.0})
	if err != nil {
		t.Fatal(err)
	}
	duration := end.Sub(start)
	if duration < 9*time.Minute+59*time.Second || duration > 10*time.Minute+time.Second {
		t.Fatalf("duration = %s, want about ten minutes", duration)
	}
}

func TestLTSPhysicalRetentionUsesLongestMetric(t *testing.T) {
	retention := map[string]int{
		"cpu.usage":       30,
		"memory.used":     90,
		"ping.latency_ms": 15,
	}
	resourceDays, pingDays := ltsPhysicalRetentionDays(retention)
	if resourceDays != 90 || pingDays != 15 {
		t.Fatalf("physical retention = (%d, %d), want (90, 15)", resourceDays, pingDays)
	}
}

func TestClampLTSHistoryRequestToRetention(t *testing.T) {
	now := time.Now()
	request := history.QueryRequest{
		Start: now.Add(-90 * 24 * time.Hour).Format(time.RFC3339),
		End:   now.Format(time.RFC3339),
	}
	clamped, ok := clampLTSHistoryRequest(request, 15)
	if !ok {
		t.Fatal("current query should overlap retention window")
	}
	start, err := time.Parse(time.RFC3339, clamped.Start)
	if err != nil {
		t.Fatal(err)
	}
	if age := time.Since(start); age < 15*24*time.Hour-time.Minute || age > 15*24*time.Hour+time.Minute {
		t.Fatalf("clamped start age = %s, want about 15 days", age)
	}

	request.End = now.Add(-20 * 24 * time.Hour).Format(time.RFC3339)
	if _, ok := clampLTSHistoryRequest(request, 15); ok {
		t.Fatal("query entirely before retention window should be skipped")
	}
}

func TestLimitLTSMetricPointsUsesTotalBudget(t *testing.T) {
	points := make([]ltsMetricPoint, 10)
	for index := range points {
		points[index].Time = time.Unix(int64(index), 0)
	}
	series := []ltsMetricSeries{
		{MetricKey: "cpu.usage", Points: append([]ltsMetricPoint(nil), points...)},
		{MetricKey: "memory.used", Points: append([]ltsMetricPoint(nil), points...)},
	}
	limited := limitLTSMetricPoints(series, 7)
	if got := len(limited[0].Points) + len(limited[1].Points); got != 7 {
		t.Fatalf("returned points = %d, want 7", got)
	}
}
