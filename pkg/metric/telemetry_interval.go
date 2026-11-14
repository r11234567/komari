package metric

import "time"

// TelemetryBucketStart shares the Unix-epoch grid used by the raw aggregation
// reference. time.Time.Truncate has a different origin for intervals like 7h.
func TelemetryBucketStart(at time.Time, interval time.Duration) time.Time {
	return alignTime(at, interval)
}

// TelemetryChartInterval chooses a grid compatible with the retained physical
// tiers, without imposing the age/precision of unrelated historical series.
// Source coverage remains a separate per-series result of the actual planner.
func TelemetryChartInterval(span time.Duration, points int) time.Duration {
	if span <= 0 || points <= 0 {
		return 10 * time.Second
	}
	quantum := telemetryResolution(span)
	// A rolling day crosses into five-minute ownership as its oldest edge
	// advances. Keep that grid stable instead of producing incompatible 4m cells.
	if span >= 24*time.Hour {
		quantum = max(quantum, 5*time.Minute)
	}
	// First-party charts aim at 360 where source precision permits it. Point
	// count is secondary to complete physical cells (a day may have 288 cells).
	if points >= 600 {
		step := max(int64(1), (int64(span)+360*int64(quantum)-1)/(360*int64(quantum)))
		if step > 1 && int64(span)/(step*int64(quantum)) < 300 {
			step--
		}
		if (int64(span)+step*int64(quantum)-1)/(step*int64(quantum)) <= int64(points) {
			return time.Duration(step) * quantum
		}
	}
	needed := (int64(span) + int64(points) - 1) / int64(points)
	return time.Duration((needed+int64(quantum)-1)/int64(quantum)) * quantum
}
