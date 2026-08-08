package jsonrpc

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/komari-monitor/komari/database/history"
)

func TestLTSMetricQueryParamsAcceptRanThemeAliases(t *testing.T) {
	var params ltsMetricQueryParams
	if err := json.Unmarshal([]byte(`{"entity_ids":["node-a","node-b"],"uuid":"node-a"}`), &params); err != nil {
		t.Fatal(err)
	}
	if len(params.EntityIDs) != 2 || params.EntityIDs[0] != "node-a" || params.UUID != "node-a" {
		t.Fatalf("Ran aliases were not decoded: %#v", params)
	}
}

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
	// Use a wider time range so rebucketing doesn't collapse everything into one bucket.
	points := make([]ltsMetricPoint, 100)
	base := time.Unix(10000, 0)
	for index := range points {
		points[index].Time = base.Add(time.Duration(index) * 10 * time.Second)
		points[index].Count = 1
		val := float64(index)
		points[index].Value = &val
	}
	series := []ltsMetricSeries{
		{MetricKey: "cpu.usage", Points: append([]ltsMetricPoint(nil), points...)},
		{MetricKey: "memory.used", Points: append([]ltsMetricPoint(nil), points...)},
	}
	limited := limitLTSMetricPoints(series, 20)
	total := len(limited[0].Points) + len(limited[1].Points)
	if total > 20 {
		t.Fatalf("returned points = %d, want at most 20", total)
	}
	if total == 0 {
		t.Fatalf("returned zero points, decimation too aggressive")
	}

	// Check that the two series have the same set of timestamps (aligned).
	if len(limited[0].Points) == 0 || len(limited[1].Points) == 0 {
		t.Fatalf("one series lost all points")
	}
	timestampsA := make(map[int64]bool)
	for _, pt := range limited[0].Points {
		timestampsA[pt.Time.Unix()] = true
	}
	for _, pt := range limited[1].Points {
		if !timestampsA[pt.Time.Unix()] {
			t.Fatalf("series have misaligned timestamps: %s not in first series", pt.Time)
		}
	}
}
