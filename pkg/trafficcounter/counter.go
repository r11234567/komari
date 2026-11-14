package trafficcounter

import (
	"math"
	"time"
)

const allowance = int64(64 * 1024 * 1024)

func Delta(current, previous int64) int64 {
	if current < 0 || previous < 0 || current < previous {
		return 0
	}
	return current - previous
}
func ReportDelta(current, previous, rate int64, elapsed time.Duration) int64 {
	d := Delta(current, previous)
	if d == 0 || elapsed <= 0 {
		return d
	}
	if d > UpperBound(rate, elapsed) {
		return 0
	}
	return d
}
func UpperBound(rate int64, elapsed time.Duration) int64 {
	if rate < 0 {
		return allowance
	}
	seconds := int64((elapsed + time.Second - 1) / time.Second)
	if seconds <= 0 || rate == 0 {
		return allowance
	}
	if rate > math.MaxInt64/seconds {
		return math.MaxInt64
	}
	expected := rate * seconds
	if expected > (math.MaxInt64-allowance)/4 {
		return math.MaxInt64
	}
	return expected*4 + allowance
}
