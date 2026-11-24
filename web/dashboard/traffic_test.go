package dashboard

import (
	"context"
	"testing"
	"time"
)

func TestDefaultTrafficTrendUsesNinetySixQuarterHours(t *testing.T) {
	buckets, err := LoadTrafficTrend(t.Context(), nil, time.Date(2026, 11, 24, 13, 7, 0, 0, time.UTC), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(buckets) != 96 || buckets[1].Start.Sub(buckets[0].Start) != 15*time.Minute ||
		buckets[95].Start.UTC().Hour() != 13 || buckets[95].Start.Minute() != 0 {
		t.Fatalf("unexpected 24-hour grid: %#v", buckets)
	}
}

func TestLoadTrafficTrendRejectsIntervalsOutsideDashboardRange(t *testing.T) {
	for _, interval := range []time.Duration{9 * time.Minute, 31 * time.Minute} {
		if _, err := LoadTrafficTrend(context.Background(), nil, time.Now(), DefaultTrafficTrendWindow, interval); err == nil {
			t.Fatalf("interval %s was accepted", interval)
		}
	}
}
