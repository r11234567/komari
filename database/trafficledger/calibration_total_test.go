package trafficledger

import (
	"context"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/komari-monitor/komari/database/models"
	"github.com/stretchr/testify/require"
)

func TestProviderTotalKeepsBillableOffsetWhenDirectionsCross(t *testing.T) {
	for _, kind := range []string{"sum", "up", "down", "max", "min", ""} {
		t.Run(kind, func(t *testing.T) {
			db := openCalibrationTestDB(t, "provider-total-"+kind)
			day := 26
			client := models.Client{UUID: "node", Token: "token", TrafficResetDay: &day, TrafficLimitType: kind}
			require.NoError(t, db.Create(&client).Error)
			now := time.Date(2026, 10, 26, 2, 0, 0, 0, time.UTC)
			raw := Usage{Up: 60000000000, Down: 59000000000}
			setMetricUsageForTest(t, func(context.Context, string, time.Time, time.Time) (Usage, error) { return raw, nil })
			target := int64(50000000000)
			first, err := CalibrateBillableCycle(context.Background(), db, client, target, "admin", now)
			require.NoError(t, err)
			require.Equal(t, target, BillableUsage(kind, first.Effective.Up, first.Effective.Down))
			require.Equal(t, raw, first.Raw)
			offset := target - BillableUsage(kind, raw.Up, raw.Down)
			require.Equal(t, offset, *first.History[0].BillableOffset)
			require.Equal(t, target, *first.History[0].TargetTotal)
			// Down becomes dominant. A pair of fixed directional offsets alone
			// gives the wrong max/min result here.
			raw.Down += 5000000000
			grown, err := LoadCalibrationSnapshot(context.Background(), db, client, now.Add(time.Minute))
			require.NoError(t, err)
			require.Equal(t, BillableUsage(kind, raw.Up, raw.Down)+offset, BillableUsage(kind, grown.Effective.Up, grown.Effective.Down))
			require.Equal(t, raw, grown.Raw)
			// A second provider observation replaces, rather than compounds,
			// the previous billable offset, including a positive correction.
			second, err := CalibrateBillableCycle(context.Background(), db, client, 150000000000, "admin", now.Add(2*time.Minute))
			require.NoError(t, err)
			require.Equal(t, int64(150000000000), BillableUsage(kind, second.Effective.Up, second.Effective.Down))
			require.Len(t, second.History, 2)
		})
	}
}

func TestTotalDirectionSwitchesAndChangedCountingType(t *testing.T) {
	db := openCalibrationTestDB(t, "provider-total-mode-switch")
	day := 26
	client := models.Client{UUID: "node", Token: "token", TrafficResetDay: &day, TrafficLimitType: "max"}
	require.NoError(t, db.Create(&client).Error)
	now := time.Date(2026, 10, 26, 2, 0, 0, 0, time.UTC)
	raw := Usage{Up: 60, Down: 59}
	setMetricUsageForTest(t, func(context.Context, string, time.Time, time.Time) (Usage, error) { return raw, nil })
	_, err := CalibrateBillableCycle(context.Background(), db, client, 50, "admin", now)
	require.NoError(t, err)
	raw.Down = 64
	snapshot, err := CalibrateCurrentCycle(context.Background(), db, client, Usage{Up: 51, Down: 52}, "admin", now)
	require.NoError(t, err)
	require.Equal(t, Usage{Up: 51, Down: 52}, snapshot.Effective)
	require.Equal(t, "directions", snapshot.History[0].Mode)
	_, err = CalibrateBillableCycle(context.Background(), db, client, 70, "admin", now)
	require.NoError(t, err)
	client.TrafficLimitType = "sum"
	snapshot, err = LoadCalibrationSnapshot(context.Background(), db, client, now)
	require.NoError(t, err)
	require.Equal(t, raw, snapshot.Effective)
	require.NotEmpty(t, snapshot.CalibrationWarning)
	snapshot, err = CalibrateBillableCycle(context.Background(), db, client, 100, "admin", now)
	require.NoError(t, err)
	require.Equal(t, int64(100), BillableUsage("sum", snapshot.Effective.Up, snapshot.Effective.Down))
	require.Empty(t, snapshot.CalibrationWarning)
}

func TestZeroDeltaModeSwitchIsRecorded(t *testing.T) {
	db := openCalibrationTestDB(t, "provider-total-zero")
	day := 26
	client := models.Client{UUID: "node", Token: "token", TrafficResetDay: &day, TrafficLimitType: "max"}
	require.NoError(t, db.Create(&client).Error)
	now := time.Date(2026, 10, 26, 2, 0, 0, 0, time.UTC)
	raw := Usage{Up: 60, Down: 59}
	setMetricUsageForTest(t, func(context.Context, string, time.Time, time.Time) (Usage, error) { return raw, nil })
	snapshot, err := CalibrateBillableCycle(context.Background(), db, client, 60, "admin", now)
	require.NoError(t, err)
	require.Len(t, snapshot.History, 1)
	snapshot, err = CalibrateCurrentCycle(context.Background(), db, client, raw, "admin", now)
	require.NoError(t, err)
	require.Len(t, snapshot.History, 2)
	require.Equal(t, "directions", snapshot.History[0].Mode)
	_, err = CalibrateBillableCycle(context.Background(), db, client, 50, "admin", now)
	require.NoError(t, err)
	next, err := LoadCalibrationSnapshot(context.Background(), db, client, now.AddDate(0, 1, 0))
	require.NoError(t, err)
	require.Empty(t, next.History)
	require.Equal(t, raw, next.Effective)
}

func TestTotalCalibrationRecoversMissingHistoryWithZeroDelta(t *testing.T) {
	db := openCalibrationTestDB(t, "provider-total-incomplete")
	openTrafficMetricStore(t, 1)
	day := 20
	client := models.Client{UUID: "node", Token: "token", TrafficResetDay: &day, TrafficResetTime: "00:05:00", TrafficLimitType: "sum"}
	require.NoError(t, db.Create(&client).Error)
	now := time.Date(2026, 10, 26, 2, 0, 0, 0, time.UTC)
	setMetricUsageForTest(t, func(context.Context, string, time.Time, time.Time) (Usage, error) { return Usage{}, nil })
	snapshot, err := LoadCalibrationSnapshot(context.Background(), db, client, now)
	require.NoError(t, err)
	require.False(t, snapshot.HistoryComplete)
	snapshot, err = CalibrateBillableCycle(context.Background(), db, client, 0, "admin", now)
	require.NoError(t, err)
	require.True(t, snapshot.HistoryComplete)
	require.Len(t, snapshot.History, 1)
	require.Equal(t, "total:sum", snapshot.History[0].Mode)
}

func TestTotalAllocationAndBillableBounds(t *testing.T) {
	for _, kind := range []string{"sum", "up", "down", "max", "min"} {
		for _, target := range []int64{0, 1, math.MaxInt64} {
			t.Run(fmt.Sprintf("%s-%d", kind, target), func(t *testing.T) {
				values, err := CalibrationTargetsForTotal(kind, Usage{Up: math.MaxInt64, Down: math.MaxInt64}, target)
				require.NoError(t, err)
				require.Equal(t, target, BillableUsage(kind, values.Up, values.Down))
			})
		}
	}
	require.Equal(t, int64(math.MaxInt64), BillableUsage("sum", math.MaxInt64, 1))
	_, err := CalibrationTargetsForTotal("sum", Usage{}, -1)
	require.Error(t, err)
}

func TestConcurrentTotalCalibrationDoesNotDoubleOffset(t *testing.T) {
	db := openCalibrationTestDB(t, "provider-total-concurrent")
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	day := 26
	client := models.Client{UUID: "node", Token: "token", TrafficResetDay: &day, TrafficLimitType: "sum"}
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
			_, err := CalibrateBillableCycle(context.Background(), db, client, 100, "admin", now)
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
	require.Equal(t, int64(100), BillableUsage("sum", snapshot.Effective.Up, snapshot.Effective.Down))
	require.Equal(t, int64(70), *snapshot.History[0].BillableOffset)
}

func TestCycleCacheDoesNotPublishAfterInvalidation(t *testing.T) {
	db := openCalibrationTestDB(t, "provider-cache-generation")
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	day := 26
	client := models.Client{UUID: "node", Token: "token", TrafficResetDay: &day}
	require.NoError(t, db.Create(&client).Error)
	now := time.Date(2026, 10, 26, 2, 0, 0, 0, time.UTC)
	InvalidateCalibratedCycleCache()
	t.Cleanup(InvalidateCalibratedCycleCache)
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	setMetricUsageForTest(t, func(context.Context, string, time.Time, time.Time) (Usage, error) {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
			return Usage{Up: 1}, nil
		}
		return Usage{Up: 2}, nil
	})
	done := make(chan error, 1)
	go func() { _, err := CurrentCalibratedCycleUsages(context.Background(), db, now); done <- err }()
	<-entered
	InvalidateCalibratedCycleCache()
	close(release)
	require.NoError(t, <-done)
	values, err := CurrentCalibratedCycleUsages(context.Background(), db, now)
	require.NoError(t, err)
	require.Equal(t, int64(2), values[client.UUID].Up)
	_, err = CurrentCalibratedCycleUsages(context.Background(), db, now)
	require.NoError(t, err)
	require.Equal(t, int32(2), calls.Load())
}

func TestCycleCacheDoesNotPublishFailedOrCanceledLoads(t *testing.T) {
	for _, cancelRequest := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel-%t", cancelRequest), func(t *testing.T) {
			db := openCalibrationTestDB(t, fmt.Sprintf("failed-cycle-cache-%t", cancelRequest))
			day := 26
			client := models.Client{UUID: "node", Token: "synthetic-token", TrafficResetDay: &day}
			require.NoError(t, db.Create(&client).Error)
			now := time.Date(2026, 10, 26, 2, 0, 0, 0, time.UTC)
			InvalidateCalibratedCycleCache()
			t.Cleanup(InvalidateCalibratedCycleCache)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var calls atomic.Int32
			setMetricUsageForTest(t, func(context.Context, string, time.Time, time.Time) (Usage, error) {
				if calls.Add(1) == 1 {
					if cancelRequest {
						cancel()
						return Usage{}, context.Canceled
					}
					return Usage{}, fmt.Errorf("synthetic transient metric read failure")
				}
				return Usage{Up: 42, Down: 21}, nil
			})
			_, err := CurrentCalibratedCycleUsages(ctx, db, now)
			require.Error(t, err, "failed reads must not be reported or cached as successful empty usage")
			if cancelRequest {
				require.ErrorIs(t, err, context.Canceled)
			}
			values, err := CurrentCalibratedCycleUsages(context.Background(), db, now)
			require.NoError(t, err)
			require.Equal(t, Usage{Up: 42, Down: 21}, values[client.UUID], "next read must reload immediately, not wait 15 seconds")
			_, err = CurrentCalibratedCycleUsages(context.Background(), db, now)
			require.NoError(t, err)
			require.Equal(t, int32(2), calls.Load(), "successful retry should still be cached")
		})
	}
}
