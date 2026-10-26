package trafficledger

import (
	"context"
	"github.com/komari-monitor/komari/database/models"
	"github.com/stretchr/testify/require"
	"sync"
	"testing"
	"time"
)

func TestProviderCalibrationKeepsOffsetAsRealTrafficGrows(t *testing.T) {
	db := openCalibrationTestDB(t, "provider-offset-growth")
	day := 26
	client := models.Client{UUID: "node", Token: "token", TrafficResetDay: &day, TrafficResetTime: "12:30:00", TrafficResetTimezone: "UTC+05:30"}
	require.NoError(t, db.Create(&client).Error)
	now := time.Date(2026, 10, 26, 8, 0, 0, 0, time.UTC)
	raw := Usage{Up: 21230000000, Down: 60000000000}
	setMetricUsageForTest(t, func(context.Context, string, time.Time, time.Time) (Usage, error) { return raw, nil })
	first, err := CalibrateCurrentCycle(context.Background(), db, client, Usage{Up: 50720000000, Down: 50000000000}, "administrator", now)
	require.NoError(t, err)
	require.Equal(t, SignedUsage{Up: 29490000000, Down: -10000000000}, first.Adjustment)
	require.Equal(t, raw, first.Raw)
	raw.Up += 5000000000
	raw.Down += 5000000000
	grown, err := LoadCalibrationSnapshot(context.Background(), db, client, now.Add(time.Minute))
	require.NoError(t, err)
	require.Equal(t, Usage{Up: 55720000000, Down: 55000000000}, grown.Effective)
	require.Equal(t, first.Adjustment, grown.Adjustment)
	corrected, err := CalibrateCurrentCycle(context.Background(), db, client, Usage{Up: 30000000000, Down: 10000000000}, "administrator", now.Add(2*time.Minute))
	require.NoError(t, err)
	require.Equal(t, Usage{Up: 30000000000, Down: 10000000000}, corrected.Effective)
	require.Len(t, corrected.History, 2)
	require.Equal(t, raw, corrected.Raw)
}

func TestConcurrentSameCalibrationDoesNotDoubleCompensation(t *testing.T) {
	db := openCalibrationTestDB(t, "provider-offset-concurrent")
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	day := 26
	client := models.Client{UUID: "node", Token: "token", TrafficResetDay: &day}
	require.NoError(t, db.Create(&client).Error)
	now := time.Date(2026, 10, 26, 2, 0, 0, 0, time.UTC)
	setMetricUsageForTest(t, func(context.Context, string, time.Time, time.Time) (Usage, error) {
		return Usage{Up: 10, Down: 20}, nil
	})
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := CalibrateCurrentCycle(context.Background(), db, client, Usage{Up: 100, Down: 200}, "administrator", now)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	snapshot, err := LoadCalibrationSnapshot(context.Background(), db, client, now)
	require.NoError(t, err)
	require.Equal(t, Usage{Up: 100, Down: 200}, snapshot.Effective)
	require.Len(t, snapshot.History, 1)
}
