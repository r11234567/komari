package dashboard

import (
	"context"
	"testing"
	"time"
)

func TestLoadTrafficTrendRejectsIntervalsOutsideDashboardRange(t *testing.T) {
	for _, interval := range []time.Duration{9 * time.Minute, 31 * time.Minute} {
		if _, err := LoadTrafficTrend(context.Background(), nil, time.Now(), DefaultTrafficTrendWindow, interval); err == nil {
			t.Fatalf("interval %s was accepted", interval)
		}
	}
}
