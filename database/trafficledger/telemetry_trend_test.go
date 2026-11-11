package trafficledger

import (
	"context"
	"testing"
	"time"

	"github.com/komari-monitor/komari/pkg/metric"
	"github.com/stretchr/testify/require"
)

// These are business-contract regressions: an HTTP 200 alone would miss the
// previous implementation assigning a whole hour to its first 20-minute bin.
func TestTelemetryTrafficTwentyMinuteBucketsPreserveDistribution(t *testing.T) {
	for _, cut := range []bool{false, true} {
		name := "whole-hour"
		if cut {
			name = "start-inside-hour"
		}
		t.Run(name, func(t *testing.T) {
			db := openLedgerTestDB(t, "telemetry-trend-"+name)
			require.NoError(t, db.AutoMigrate(TelemetryModels()...))
			at := time.Date(2026, 11, 10, 10, 0, 0, 0, BeijingLocation)
			var total int64
			for i, delta := range []int64{100, 200, 300} {
				total += delta
				observed := at.Add(time.Duration(i*20+5) * time.Minute)
				require.NoError(t, ApplyTelemetry(context.Background(), db, metric.TelemetryTrafficEvent{
					StoreID: "trend", Source: "client-a", IngestID: int64(i + 1), Observed: observed, Received: observed,
					Traffic: metric.TelemetryTraffic{TotalUp: total, TotalDown: total * 2, HasDelta: true, DeltaUp: delta, DeltaDown: delta * 2},
				}))
			}
			start := at
			want := []int64{100, 200, 300}
			if cut {
				start = start.Add(20 * time.Minute)
				want = want[1:]
			}
			totals, buckets, err := TelemetryUsageBatch(context.Background(), db, []string{"client-a"}, start, at.Add(time.Hour).Add(-time.Nanosecond), 20*time.Minute)
			require.NoError(t, err)
			require.Len(t, buckets["client-a"], len(want))
			var sum int64
			for i, expected := range want {
				require.Equal(t, start.Add(time.Duration(i)*20*time.Minute), buckets["client-a"][i].Hour)
				require.Equal(t, Usage{Up: expected, Down: expected * 2}, buckets["client-a"][i].Usage)
				sum += expected
			}
			require.Equal(t, Usage{Up: sum, Down: sum * 2}, totals["client-a"])
		})
	}
}
