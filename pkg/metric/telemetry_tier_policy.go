package metric

import "time"

// Keep the importer, online promotion, pruning and chart grid on one nested
// hierarchy. The final tier expires with the metric itself (keep == 0).
// Short-lived fine cells must not become a second permanent raw store.
var telemetrySummaryTiers = [...]struct{ resolution, keep time.Duration }{
	{10 * time.Second, time.Hour},
	{time.Minute, 24 * time.Hour},
	{5 * time.Minute, 2 * 24 * time.Hour},
	{30 * time.Minute, 7 * 24 * time.Hour},
	{time.Hour, 15 * 24 * time.Hour},
	{2 * time.Hour, 60 * 24 * time.Hour},
	{6 * time.Hour, 180 * 24 * time.Hour},
	{12 * time.Hour, 365 * 24 * time.Hour},
	{24 * time.Hour, 0},
}

func telemetryResolution(age time.Duration) time.Duration {
	for _, tier := range telemetrySummaryTiers {
		if tier.keep == 0 || age <= tier.keep {
			return tier.resolution
		}
	}
	return 24 * time.Hour
}

// Promotion prepares a complete parent near child expiry. Cap the preparation
// window to one eighth of child retention: a two-hour safety lead must never
// immediately promote newly written ten-second cells with a one-hour life.
func telemetryPromotionLead(keep, target time.Duration) time.Duration {
	return min(max(2*target, 2*time.Hour), keep/8)
}

// Previous v2 stores can contain 15m cells. They are a source to retire, not a
// retained tier to keep generating. The old v1 writer also needs its terminal
// hour cells promoted before the new age policy may reclaim them.
func telemetryPromotionTiers() []struct{ source, target, keep time.Duration } {
	var out []struct{ source, target, keep time.Duration }
	for i := 0; i+1 < len(telemetrySummaryTiers); i++ {
		tier := telemetrySummaryTiers[i]
		out = append(out, struct{ source, target, keep time.Duration }{tier.resolution, telemetrySummaryTiers[i+1].resolution, tier.keep})
	}
	return append(out, struct{ source, target, keep time.Duration }{15 * time.Minute, 30 * time.Minute, 2 * 24 * time.Hour})
}

// An imported nonstandard interval cannot be split. Expire it at the same
// deadline as the finer tier it replaces, then use an LCM to preserve its
// complete observations when promoting it to the next retained grid.
func telemetrySummaryKeep(resolution time.Duration) time.Duration {
	for i := 0; i+1 < len(telemetrySummaryTiers); i++ {
		if resolution < telemetrySummaryTiers[i+1].resolution {
			return telemetrySummaryTiers[i].keep
		}
	}
	return 0
}
