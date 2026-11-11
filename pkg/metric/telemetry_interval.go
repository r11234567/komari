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
	quantum := 10 * time.Second
	switch {
	case span > 14*24*time.Hour:
		quantum = time.Hour
	case span > 48*time.Hour:
		quantum = 15 * time.Minute
	case span > 3*time.Hour:
		quantum = time.Minute
	}
	needed := (int64(span) + int64(points) - 1) / int64(points)
	return time.Duration((needed+int64(quantum)-1)/int64(quantum)) * quantum
}
