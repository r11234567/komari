package trafficledger

import (
	"context"
	"errors"
	"time"

	"github.com/komari-monitor/komari/database/metricstore"
	"github.com/komari-monitor/komari/database/models"
	"gorm.io/gorm"
)

var ErrCalibrationCatchingUp = errors.New("traffic accounting is catching up; retry calibration after pending reports are committed")

// Acquire the metric lifecycle lease before the accounting mutex, matching the
// background delivery order. Never drain the outbox from an administrator save.
// New reports accepted after this check belong to subsequent counter growth.
func withCalibrationAccounting(ctx context.Context, save func() (CalibrationSnapshot, error)) (CalibrationSnapshot, error) {
	if !metricstore.TelemetryEnabled() {
		return save()
	}
	s, release, err := metricstore.AcquireStore(ctx)
	if err != nil {
		return CalibrationSnapshot{}, err
	}
	defer release()
	telemetryTrafficMu.Lock()
	defer telemetryTrafficMu.Unlock()
	if t := s.Telemetry(); t != nil {
		pending, err := t.TrafficPending(ctx)
		if err != nil {
			return CalibrationSnapshot{}, err
		}
		if pending {
			return CalibrationSnapshot{}, ErrCalibrationCatchingUp
		}
	}
	return save()
}

// Calibration reads business ledgers, independently of minute/hour chart
// coverage and raw retention. Non-midnight cycle starts use the separately
// maintained first-day ownership (also preserved by offline migration).
func telemetryCurrentCalibrationUsage(ctx context.Context, db *gorm.DB, client models.Client, start, now time.Time) (Usage, error) {
	if !start.Equal(BeijingDay(now)) {
		_, cycle, err := CurrentTrafficCycleFor(client, now)
		if err != nil {
			return Usage{}, err
		}
		row, _, err := loadStoredCycleFirstDay(ctx, db, client.UUID, cycle)
		return firstDayUsage(row), err
	}
	var row models.TrafficDailyLedger
	err := db.WithContext(ctx).Where("client = ? AND day = ?", client.UUID, dayKey(now)).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Usage{}, nil
	}
	return Usage{Up: row.UpBytes, Down: row.DownBytes}, err
}
