package trafficledger

import (
	"math/bits"
	"time"

	"github.com/komari-monitor/komari/pkg/trafficreset"
)

const maxTrafficInterpolation = 2 * time.Minute

type trafficAllocation struct {
	at    time.Time
	usage Usage
}

// allocateTraffic forms a partition of [start, end). An observation on a
// boundary closes its previous interval. Minute slices use the same deltas
// as the cycle ledger, including for a short interval across a cycle boundary.
func allocateTraffic(schedule trafficreset.Schedule, start, end time.Time, usage Usage) ([]trafficAllocation, string) {
	return allocateTrafficRange(policyRange{legacy: schedule}, start, end, usage)
}

func allocateTrafficRange(policies policyRange, start, end time.Time, usage Usage) ([]trafficAllocation, string) {
	if start.IsZero() || !start.Before(end) {
		return []trafficAllocation{{at: end, usage: usage}}, ""
	}
	last := end.Add(-time.Nanosecond)
	initial, final := policies.at(start), policies.at(last)
	crossesCycle := !initial.StartsAt.Equal(final.StartsAt) || initial.CycleKey(start) != final.CycleKey(last)
	if end.Sub(start) > maxTrafficInterpolation {
		if crossesCycle {
			return nil, "unallocated"
		}
		return []trafficAllocation{{at: last, usage: usage}}, "long_interval"
	}
	var parts []trafficAllocation
	var allocated Usage
	for cursor := start; cursor.Before(end); {
		boundary := minInstant(cursor.Truncate(time.Minute).Add(time.Minute), end)
		schedule := policies.at(cursor)
		if reset := schedule.Next(cursor); !reset.IsZero() {
			boundary = minInstant(boundary, reset)
		}
		if !schedule.EndsAt.IsZero() {
			boundary = minInstant(boundary, schedule.EndsAt)
		}
		cumulative := Usage{
			Up:   trafficProportion(usage.Up, boundary.Sub(start), end.Sub(start)),
			Down: trafficProportion(usage.Down, boundary.Sub(start), end.Sub(start)),
		}
		parts = append(parts, trafficAllocation{at: boundary.Add(-time.Nanosecond), usage: Usage{Up: cumulative.Up - allocated.Up, Down: cumulative.Down - allocated.Down}})
		cursor, allocated = boundary, cumulative
	}
	if crossesCycle {
		return parts, "estimated"
	}
	return parts, ""
}

func minInstant(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func trafficProportion(bytes int64, portion, total time.Duration) int64 {
	hi, lo := bits.Mul64(uint64(bytes), uint64(portion))
	q, _ := bits.Div64(hi, lo, uint64(total))
	return int64(q)
}
