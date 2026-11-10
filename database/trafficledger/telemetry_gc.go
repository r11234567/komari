package trafficledger

import (
	"context"
	"time"

	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/pkg/metric"
	"gorm.io/gorm"
)

// PruneTelemetrySlices retains 35 days of hourly drilldown independently from
// lifetime counters, daily ledgers, billing cycles and calibration history.
func PruneTelemetrySlices(ctx context.Context, now time.Time, limit int) (int, error) {
	if limit < 1 || limit > 512 {
		return 0, metric.ErrInvalidArgument
	}
	db := telemetryMetadata.Load()
	if db == nil {
		return 0, nil
	}
	var rows []models.TelemetryTrafficSlice
	if err := db.WithContext(ctx).Where("end < ?", now.AddDate(0, 0, -35).UnixNano()).Order("end, client, start").Limit(limit).Find(&rows).Error; err != nil {
		return 0, err
	}
	for _, row := range rows {
		if err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			if err := tx.Model(&models.TelemetryTrafficState{}).Where("client = ?", row.Client).UpdateColumn("hourly_pruned_through", gorm.Expr("CASE WHEN hourly_pruned_through < ? THEN ? ELSE hourly_pruned_through END", row.End, row.End)).Error; err != nil {
				return err
			}
			return tx.Where("client = ? AND start = ?", row.Client, row.Start).Delete(&models.TelemetryTrafficSlice{}).Error
		}); err != nil {
			return 0, err
		}
	}
	return len(rows), nil
}
