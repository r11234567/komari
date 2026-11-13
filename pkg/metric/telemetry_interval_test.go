package metric

import (
	"testing"
	"time"
)

func TestTelemetryChartIntervalWindows(t *testing.T) {
	for _, tc := range []struct {
		hours    int
		points   int
		interval time.Duration
	}{
		{1, 360, 10 * time.Second}, {6, 360, time.Minute}, {12, 360, 2 * time.Minute},
		{24, 360, 4 * time.Minute}, {168, 360, 30 * time.Minute}, {360, 360, time.Hour},
		{720, 360, 2 * time.Hour}, {1080, 360, 4 * time.Hour}, {1440, 360, 4 * time.Hour},
		{1800, 360, 6 * time.Hour}, {2160, 309, 12 * time.Hour}, {2520, 360, 12 * time.Hour},
		{1080, 600, 2 * time.Hour}, {1800, 600, 6 * time.Hour}, {2160, 600, 6 * time.Hour},
		{2520, 600, 6 * time.Hour}, {2880, 600, 6 * time.Hour}, {4320, 600, 12 * time.Hour},
		{8760, 600, 24 * time.Hour},
	} {
		if got := TelemetryChartInterval(time.Duration(tc.hours)*time.Hour, tc.points); got != tc.interval {
			t.Fatalf("%dh: got %s want %s", tc.hours, got, tc.interval)
		}
	}
}
