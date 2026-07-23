package jsonrpc

import (
	"testing"
	"time"
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
