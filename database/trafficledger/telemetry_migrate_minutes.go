package trafficledger

import (
	"context"
	"time"

	"github.com/komari-monitor/komari/database/metricstore"
	"github.com/komari-monitor/komari/database/models"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// This is a replacement precision index for the same hourly bytes, never a
// second posting to daily/cycle billing. Only a complete, byte-reconciled raw
// population may advertise minute coverage.
func refineLegacyTrafficWindow(ctx context.Context, db *gorm.DB, client string, start, end, cutoff time.Time, want Usage) (bool, error) {
	if start.Before(cutoff.Add(-telemetryMinuteRetention)) || !start.Equal(start.Truncate(time.Minute)) {
		return false, nil
	}
	events, complete, err := metricstore.LegacyTrafficDeltas(ctx, client, start, end.Add(-time.Nanosecond))
	if err != nil || !complete {
		return false, err
	}
	var total Usage
	minutes := map[int64]*models.TelemetryTrafficMinute{}
	var tails []models.TelemetryTrafficTail
	for _, event := range events {
		total.Up = saturatingAdd(total.Up, event.Up)
		total.Down = saturatingAdd(total.Down, event.Down)
		ns := event.Observed.UnixNano()
		key := event.Observed.Truncate(time.Minute).UnixNano()
		row := minutes[key]
		if row == nil {
			row = &models.TelemetryTrafficMinute{Client: client, Start: key, FirstObserved: ns}
			minutes[key] = row
		}
		row.LastObserved = ns
		row.UpBytes = saturatingAdd(row.UpBytes, event.Up)
		row.DownBytes = saturatingAdd(row.DownBytes, event.Down)
		if !event.Observed.Before(cutoff.Add(-telemetryTailRetention)) {
			tails = append(tails, models.TelemetryTrafficTail{Client: client, Observed: ns, UpBytes: event.Up, DownBytes: event.Down})
		}
	}
	if total != want {
		return false, nil
	}
	err = db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("client=? AND start>=? AND start<?", client, start.UnixNano(), end.UnixNano()).Delete(&models.TelemetryTrafficMinute{}).Error; err != nil {
			return err
		}
		if err := tx.Where("client=? AND observed>=? AND observed<?", client, start.UnixNano(), end.UnixNano()).Delete(&models.TelemetryTrafficTail{}).Error; err != nil {
			return err
		}
		for _, row := range minutes {
			if err := tx.Clauses(clause.OnConflict{UpdateAll: true}).Create(row).Error; err != nil {
				return err
			}
		}
		if len(tails) > 0 {
			return tx.Clauses(clause.OnConflict{UpdateAll: true}).CreateInBatches(tails, 128).Error
		}
		return nil
	})
	return err == nil, err
}
