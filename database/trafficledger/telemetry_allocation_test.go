package trafficledger

import (
	"testing"
	"time"

	"github.com/komari-monitor/komari/pkg/trafficreset"
	"github.com/stretchr/testify/require"
)

func TestTrafficAllocationConservesBytesAcrossReset(t *testing.T) {
	day := 26
	schedule := trafficreset.FromFields(&day, "15:30", "UTC+08:00")
	boundary := time.Date(2026, time.November, 26, 7, 30, 0, 0, time.UTC)
	start, end := boundary.Add(-10*time.Second), boundary.Add(10*time.Second)
	parts, result := allocateTraffic(schedule, start, end, Usage{Up: 121, Down: 39})
	require.Equal(t, "estimated", result)
	require.Len(t, parts, 2)
	require.Equal(t, Usage{Up: 60, Down: 19}, parts[0].usage)
	require.Equal(t, Usage{Up: 61, Down: 20}, parts[1].usage)
	require.NotEqual(t, schedule.CycleKey(parts[0].at), schedule.CycleKey(parts[1].at))
	parts, result = allocateTraffic(schedule, start, boundary, Usage{Up: 121})
	require.Empty(t, result)
	require.Len(t, parts, 1)
	require.Equal(t, schedule.CycleKey(start), schedule.CycleKey(parts[0].at))
	parts, result = allocateTraffic(schedule, boundary.Add(-time.Hour), end, Usage{Up: 121})
	require.Empty(t, parts)
	require.Equal(t, "unallocated", result)
}
