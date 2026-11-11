package trafficledger

import (
	"context"
	"fmt"
	"time"

	"github.com/komari-monitor/komari/database/metricstore"
	"github.com/komari-monitor/komari/database/models"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// MigrateTelemetryLedger is an explicit offline bridge, not a runtime repair.
// Every hour is bounded; reruns replace the same compact ownership rows. Old
// settled daily rows and calibration adjustments remain authoritative.
func MigrateTelemetryLedger(ctx context.Context, db *gorm.DB, now time.Time) error {
	if metricstore.TelemetryEnabled() {
		return fmt.Errorf("traffic migration requires the offline legacy reader")
	}
	var clients []models.Client
	if err := db.WithContext(ctx).Find(&clients).Error; err != nil {
		return err
	}
	today := BeijingDay(now)
	for _, client := range clients {
		if err := EnsureRange(ctx, db, []string{client.UUID}, today.AddDate(0, 0, -35), today); err != nil {
			return err
		}
		var total Usage
		var first time.Time
		var minuteSince time.Time
		for from := today.AddDate(0, 0, -35); !from.After(now); {
			until := from.Truncate(time.Hour).Add(time.Hour)
			schedule := clientSchedule(client)
			if schedule.Active() {
				if reset := schedule.Next(from); reset.After(from) && reset.Before(until) {
					until = reset
				}
			}
			until = minTimeTraffic(until, now.Add(time.Nanosecond))
			usage, err := MetricUsage(ctx, client.UUID, from, until.Add(-time.Nanosecond))
			if err != nil {
				return err
			}
			// Keep even zero rows: they establish the old hourly query contract.
			row := models.TelemetryTrafficSlice{Client: client.UUID, Start: from.UnixNano(), End: until.UnixNano(), FirstObserved: from.UnixNano(), LastObserved: until.Add(-time.Nanosecond).UnixNano(), UpBytes: usage.Up, DownBytes: usage.Down}
			if err = db.WithContext(ctx).Clauses(clause.OnConflict{UpdateAll: true}).Create(&row).Error; err != nil {
				return err
			}
			if !from.Before(now.Add(-telemetryMinuteRetention)) {
				fine, err := refineLegacyTrafficWindow(ctx, db, client.UUID, from, until, now, usage)
				if err != nil {
					return err
				}
				if fine && minuteSince.IsZero() {
					minuteSince = from
				}
				if !fine {
					minuteSince = until.Truncate(time.Minute)
					if minuteSince.Before(until) {
						minuteSince = minuteSince.Add(time.Minute)
					}
				}
			}
			if !from.Before(today) {
				total = addUsage(total, usage)
			}
			if first.IsZero() {
				first = from
			}
			from = until
		}
		day := models.TrafficDailyLedger{Client: client.UUID, Day: dayKey(today), UpBytes: total.Up, DownBytes: total.Down}
		if err := db.WithContext(ctx).Clauses(clause.OnConflict{UpdateAll: true}).Create(&day).Error; err != nil {
			return err
		}
		if _, _, err := syncCycleFirstDay(ctx, db, client, now); err != nil {
			return err
		}
		// An hourly migration reader preserves the previous API's calculation
		// and baseline recovery. Exact counter baseline comes from the source.
		at, up, down, found, err := metricstore.LegacyTrafficBaseline(ctx, client.UUID, now)
		if err != nil {
			return err
		}
		if found {
			state := models.TelemetryTrafficState{Client: client.UUID, FirstObserved: first.UnixNano(), LastObserved: at.UnixNano(), TotalUp: up, TotalDown: down}
			if !minuteSince.IsZero() && !minuteSince.After(at) {
				state.MinuteSince = minuteSince.UnixNano()
				state.TailPrunedThrough = now.Add(-telemetryTailRetention).UnixNano()
			}
			if err = db.WithContext(ctx).Clauses(clause.OnConflict{UpdateAll: true}).Create(&state).Error; err != nil {
				return err
			}
			if err = metricstore.SeedTelemetryTrafficBaseline(ctx, client.UUID, at, up, down); err != nil {
				return err
			}
		}
	}
	return nil
}

func minTimeTraffic(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
