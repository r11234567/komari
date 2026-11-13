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
	// First-party charts use a 600-point budget but aim at 360. Prefer a
	// compatible finer grid over dropping a populated preset below 300 cells.
	// Explicit smaller budgets remain hard limits. Never split a physical cell.
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
