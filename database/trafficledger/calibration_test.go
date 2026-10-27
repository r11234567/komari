package trafficledger

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/komari-monitor/komari/database/metricstore"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/pkg/metric"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func intPointer(value int) *int { return &value }

func TestCalibrationSnapshotUsesFrontendTrafficFieldNames(t *testing.T) {
	payload, err := json.Marshal(CalibrationSnapshot{
		Raw:       Usage{Up: 1, Down: 2},
		Effective: Usage{Up: 3, Down: 4},
	})
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"client":"",
		"cycle":"",
		"cycle_start":"0001-01-01T00:00:00Z",
		"cycle_end":"0001-01-01T00:00:00Z",
		"raw":{"up":1,"down":2},
		"adjustment":{"up":0,"down":0},
		"effective":{"up":3,"down":4},
		"history":null,
		"history_complete":false
	}`, string(payload))
}

func TestCurrentTrafficCycleClampsResetDayAtMonthEnd(t *testing.T) {
	resetDay := intPointer(31)
	start, cycle, err := CurrentTrafficCycle(resetDay, time.Date(2026, 2, 28, 12, 0, 0, 0, BeijingLocation))
	require.NoError(t, err)
	assert.Equal(t, "2026-02-28", cycle)
	assert.Equal(t, time.Date(2026, 2, 28, 0, 0, 0, 0, BeijingLocation), start)

	start, cycle, err = CurrentTrafficCycle(resetDay, time.Date(2026, 2, 27, 23, 59, 0, 0, BeijingLocation))
	require.NoError(t, err)
	assert.Equal(t, "2026-01-31", cycle)
	assert.Equal(t, time.Date(2026, 1, 31, 0, 0, 0, 0, BeijingLocation), start)
}

func TestNextCycleStartFollowsResetDay(t *testing.T) {
	assert.Equal(t,
		time.Date(2026, 9, 15, 0, 0, 0, 0, BeijingLocation),
		NextCycleStart(time.Date(2026, 8, 15, 0, 0, 0, 0, BeijingLocation), 15),
	)
	assert.Equal(t,
		time.Date(2026, 2, 28, 0, 0, 0, 0, BeijingLocation),
		NextCycleStart(time.Date(2026, 1, 31, 0, 0, 0, 0, BeijingLocation), 31),
	)
}

func TestTrafficCycleInclusiveEndUsesTheDayBeforeTheNextReset(t *testing.T) {
	assert.Equal(t,
		time.Date(2026, 2, 27, 0, 0, 0, 0, BeijingLocation),
		trafficCycleInclusiveEnd(time.Date(2026, 1, 31, 0, 0, 0, 0, BeijingLocation), 31),
	)
	assert.Equal(t,
		time.Date(2026, 3, 30, 0, 0, 0, 0, BeijingLocation),
		trafficCycleInclusiveEnd(time.Date(2026, 2, 28, 0, 0, 0, 0, BeijingLocation), 31),
	)
}

func TestCalibrationStopsApplyingAfterTheNextResetDay(t *testing.T) {
	resetDay := intPointer(15)
	assert.True(t, calibrationAppliesToCurrentCycle(resetDay, "2026-07-15", time.Date(2026, 8, 14, 23, 59, 0, 0, BeijingLocation)))
	assert.False(t, calibrationAppliesToCurrentCycle(resetDay, "2026-07-15", time.Date(2026, 8, 15, 0, 0, 0, 0, BeijingLocation)))
	assert.True(t, calibrationAppliesToCurrentCycle(resetDay, "2026-08-15", time.Date(2026, 8, 15, 0, 0, 0, 0, BeijingLocation)))
}

func TestAllocateNegativeCalibrationWalksBackwardWithoutNegativeDays(t *testing.T) {
	days := []calibrationDay{
		{Day: "2026-08-01", Effective: Usage{Up: 100}},
		{Day: "2026-08-02", Effective: Usage{Up: 70}},
		{Day: "2026-08-03", Effective: Usage{Up: 50}},
	}
	allocation, err := allocateCalibration(days, -120, func(day calibrationDay) int64 { return day.Effective.Up })
	require.NoError(t, err)
	assert.Equal(t, int64(-50), allocation["2026-08-03"])
	assert.Equal(t, int64(-70), allocation["2026-08-02"])
	assert.Zero(t, allocation["2026-08-01"])

	total := int64(0)
	for _, day := range days {
		value := addSignedNonNegative(day.Effective.Up, allocation[day.Day])
		assert.GreaterOrEqual(t, value, int64(0))
		total += value
	}
	assert.Equal(t, int64(100), total)
}

func TestAdjustedLedgerUsageKeepsDailyAndRangeTotalsConsistent(t *testing.T) {
	db := openLedgerTestDB(t, "calibration-ledger-consistency")
	rows := []models.TrafficDailyLedger{
		{Client: "client-a", Day: "2026-08-01", UpBytes: 100, DownBytes: 200},
		{Client: "client-a", Day: "2026-08-02", UpBytes: 300, DownBytes: 400},
	}
	require.NoError(t, db.Create(&rows).Error)
	adjustments := []models.TrafficCalibrationAdjustment{
		{CalibrationID: "a", Client: "client-a", Cycle: "2026-08-01", Day: "2026-08-01", UpDelta: 50, DownDelta: -25},
		{CalibrationID: "b", Client: "client-a", Cycle: "2026-08-01", Day: "2026-08-02", UpDelta: -75, DownDelta: 100},
	}
	require.NoError(t, db.Create(&adjustments).Error)
	start := time.Date(2026, 8, 1, 0, 0, 0, 0, BeijingLocation)
	middle := start.AddDate(0, 0, 1)
	end := start.AddDate(0, 0, 2)

	first, err := AdjustedLedgerUsage(context.Background(), db, "client-a", start, middle)
	require.NoError(t, err)
	second, err := AdjustedLedgerUsage(context.Background(), db, "client-a", middle, end)
	require.NoError(t, err)
	total, err := AdjustedLedgerUsage(context.Background(), db, "client-a", start, end)
	require.NoError(t, err)
	assert.Equal(t, Usage{Up: first.Up + second.Up, Down: first.Down + second.Down}, total)
	assert.Equal(t, Usage{Up: 375, Down: 675}, total)
}

func TestShiftCumulativeCounterMakesNewestPointExact(t *testing.T) {
	assert.Equal(t, int64(180), ShiftCumulativeCounter(130, 150, 200))
	assert.Equal(t, int64(0), ShiftCumulativeCounter(20, 100, 50))
	assert.Equal(t, int64(50), ShiftCumulativeCounter(100, 100, 50))
}

func TestCalibrationDaysIncludeBeijingTodayWhenVendorResetIsLaterTheSameDay(t *testing.T) {
	now := time.Date(2026, 9, 21, 14, 0, 0, 0, time.UTC)
	today := BeijingDay(now)
	cycleStart := time.Date(2026, 9, 21, 12, 38, 12, 0, time.UTC)
	require.True(t, cycleStart.After(today), "vendor reset after Beijing midnight is the production panic condition")

	old := map[string]*calibrationDay{}
	for day := cycleStart; !day.After(today); day = day.AddDate(0, 0, 1) {
		old[dayKey(day)] = &calibrationDay{Day: dayKey(day)}
	}
	require.Nil(t, old[dayKey(today)], "the previous AddDate loop skipped Beijing today")

	daysByKey, dayKeys := calibrationDaysByKey(cycleStart, today)
	require.NotNil(t, daysByKey[dayKey(today)])
	require.NotPanics(t, func() {
		applyCurrentDayUsage(daysByKey, dayKeys, today, Usage{Up: 7, Down: 9})
	})
	assert.Equal(t, int64(7), daysByKey[dayKey(today)].Raw.Up)
}

func TestApplyCurrentDayUsageCreatesMissingBeijingTodaySlot(t *testing.T) {
	today := BeijingDay(time.Date(2026, 9, 21, 2, 52, 0, 0, BeijingLocation))
	daysByKey := map[string]*calibrationDay{}
	require.NotPanics(t, func() {
		applyCurrentDayUsage(daysByKey, nil, today, Usage{Up: 3})
	})
	require.NotNil(t, daysByKey[dayKey(today)])
	assert.Equal(t, int64(3), daysByKey[dayKey(today)].Raw.Up)
}

func TestCurrentTrafficCycleForEmptyResetClockAndTimezone(t *testing.T) {
	day := 15
	now := time.Date(2026, 9, 21, 2, 52, 0, 0, BeijingLocation)
	start, cycle, err := CurrentTrafficCycleFor(models.Client{
		UUID:                 "node",
		TrafficResetDay:      &day,
		TrafficResetTime:     "",
		TrafficResetTimezone: "",
	}, now)
	require.NoError(t, err)
	assert.Equal(t, "2026-09-15", cycle)
	assert.True(t, start.Equal(time.Date(2026, 9, 15, 0, 0, 0, 0, BeijingLocation)))
}

func TestCurrentCalibratedCycleUsagesEmptyResetClockDoesNotPanic(t *testing.T) {
	db := openLedgerTestDB(t, "calibration-empty-reset-clock")
	// This test exercises the legacy empty-clock default, not unavailable
	// metric storage. Supply a valid zero usage instead of relying on swallowed
	// metric-store errors to make an empty cache look successful.
	setMetricUsageForTest(t, func(context.Context, string, time.Time, time.Time) (Usage, error) {
		return Usage{}, nil
	})
	InvalidateCalibratedCycleCache()
	t.Cleanup(InvalidateCalibratedCycleCache)

	day := 21
	require.NoError(t, db.Model(&models.Client{}).Where("uuid = ?", "client-a").Updates(map[string]any{
		"traffic_reset_day":      day,
		"traffic_reset_time":     "",
		"traffic_reset_timezone": "",
	}).Error)

	now := time.Date(2026, 9, 21, 14, 0, 0, 0, time.UTC)
	_, cycle, err := CurrentTrafficCycleFor(models.Client{
		UUID:                 "client-a",
		TrafficResetDay:      &day,
		TrafficResetTime:     "",
		TrafficResetTimezone: "",
	}, now)
	require.NoError(t, err)
	require.NoError(t, db.Create(&models.TrafficCalibrationAdjustment{
		CalibrationID: "cal-empty",
		Client:        "client-a",
		Cycle:         cycle,
		Day:           "2026-09-21",
		UpDelta:       1,
	}).Error)

	var usages map[string]Usage
	require.NotPanics(t, func() {
		var loadErr error
		usages, loadErr = CurrentCalibratedCycleUsages(context.Background(), db, now)
		require.NoError(t, loadErr)
	})
	assert.NotNil(t, usages)
	assert.Equal(t, Usage{Up: 1}, usages["client-a"])
}

func TestCycleEndDayDoesNotDereferenceNilResetDay(t *testing.T) {
	start := time.Date(2026, 9, 15, 0, 0, 0, 0, BeijingLocation)
	require.NotPanics(t, func() {
		end := cycleEndDay(models.Client{}, start)
		assert.False(t, end.IsZero())
	})
}

func TestCycleClosedEndUsesPreciseNextBoundary(t *testing.T) {
	day := 15
	schedule := clientSchedule(models.Client{
		TrafficResetDay:      &day,
		TrafficResetTime:     "12:38:12",
		TrafficResetTimezone: "UTC",
	})
	start := time.Date(2026, 9, 15, 12, 38, 12, 0, time.UTC)
	end := cycleClosedEnd(schedule, start)
	assert.Equal(t, time.Date(2026, 10, 15, 12, 38, 11, 999999999, time.UTC), end.UTC())

	defaultStart := time.Date(2026, 8, 15, 0, 0, 0, 0, BeijingLocation)
	defaultEnd := cycleEndDay(models.Client{TrafficResetDay: &day}, defaultStart)
	assert.Equal(t, time.Date(2026, 9, 14, 23, 59, 59, 999999999, BeijingLocation).UTC(), defaultEnd.UTC())
}

func TestIsBeijingMidnightMatchesDefaultPlan(t *testing.T) {
	assert.True(t, isBeijingMidnight(time.Date(2026, 9, 15, 0, 0, 0, 0, BeijingLocation)))
	assert.False(t, isBeijingMidnight(time.Date(2026, 9, 15, 12, 38, 12, 0, time.UTC)))
}

func TestCurrentTrafficCycleForCustomClockUsesRFC3339Key(t *testing.T) {
	day := 15
	now := time.Date(2026, 9, 15, 14, 0, 0, 0, time.UTC)
	start, cycle, err := CurrentTrafficCycleFor(models.Client{
		TrafficResetDay:      &day,
		TrafficResetTime:     "12:38:12",
		TrafficResetTimezone: "UTC",
	}, now)
	require.NoError(t, err)
	assert.Equal(t, "2026-09-15T12:38:12Z", cycle)
	assert.True(t, start.Equal(time.Date(2026, 9, 15, 12, 38, 12, 0, time.UTC)))

	_, midnightKey, err := CurrentTrafficCycleFor(models.Client{
		TrafficResetDay:      &day,
		TrafficResetTime:     "00:00:00",
		TrafficResetTimezone: "UTC",
	}, now)
	require.NoError(t, err)
	_, afternoonKey, err := CurrentTrafficCycleFor(models.Client{
		TrafficResetDay:      &day,
		TrafficResetTime:     "12:38:12",
		TrafficResetTimezone: "UTC",
	}, now)
	require.NoError(t, err)
	assert.NotEqual(t, midnightKey, afternoonKey)

	_, defaultKey, err := CurrentTrafficCycleFor(models.Client{
		TrafficResetDay:      &day,
		TrafficResetTime:     "",
		TrafficResetTimezone: "",
	}, now)
	require.NoError(t, err)
	assert.Equal(t, "2026-09-15", defaultKey)
}

func TestCalibrationDaysSkipLedgerBeforeCustomResetClock(t *testing.T) {
	cycleStart := time.Date(2026, 9, 15, 12, 38, 12, 0, time.UTC)
	today := BeijingDay(time.Date(2026, 9, 16, 8, 0, 0, 0, time.UTC))
	startDay := BeijingDay(cycleStart)
	ledgerStart := startDay
	if !isBeijingMidnight(cycleStart) {
		ledgerStart = startDay.AddDate(0, 0, 1)
	}
	assert.True(t, ledgerStart.Equal(startDay.AddDate(0, 0, 1)))
	assert.True(t, ledgerStart.Equal(BeijingDay(today)))
	currentStart := today
	if cycleStart.After(currentStart) {
		currentStart = cycleStart
	}
	assert.True(t, currentStart.Equal(today))
}

func openCalibrationTestDB(t *testing.T, name string) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared&_foreign_keys=on", name)), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(
		&models.Client{},
		&models.TrafficDailyLedger{},
		&models.TrafficCycleFirstDay{},
		&models.TrafficCalibrationAdjustment{},
	))
	return db
}

func setMetricUsageForTest(t *testing.T, fn usageCalculator) {
	t.Helper()
	previous := metricUsageForCalibration
	metricUsageForCalibration = fn
	t.Cleanup(func() { metricUsageForCalibration = previous })
}

func TestCalibrationKeepsFirstDayAfterMetricRetentionExpires(t *testing.T) {
	for _, retentionDays := range []int{1, 3, 7} {
		t.Run(fmt.Sprintf("retention_%dd", retentionDays), func(t *testing.T) {
			db := openCalibrationTestDB(t, fmt.Sprintf("calibration-first-day-%d", retentionDays))
			day := 20
			client := models.Client{
				UUID: fmt.Sprintf("node-%d", retentionDays), Token: fmt.Sprintf("token-%d", retentionDays),
				TrafficResetDay: &day, TrafficResetTime: "00:05:00", TrafficResetTimezone: "Asia/Shanghai",
			}
			require.NoError(t, db.Create(&client).Error)

			cycleStart := time.Date(2026, 9, 20, 0, 5, 0, 0, BeijingLocation)
			early := usagePoint{at: cycleStart.Add(time.Minute), up: 100, down: 200}
			mid := usagePoint{at: cycleStart.Add(12 * time.Hour), up: 30, down: 40}
			late := usagePoint{at: cycleStart.Add(22 * time.Hour), up: 7, down: 8}
			store := &retainedUsage{
				points:    []usagePoint{early, mid, late},
				retention: time.Duration(retentionDays) * 24 * time.Hour,
			}
			setMetricUsageForTest(t, store.usage)

			store.now = time.Date(2026, 9, 20, 0, 30, 0, 0, BeijingLocation)
			require.NoError(t, persistCycleFirstDays(context.Background(), db, store.now))
			store.now = time.Date(2026, 9, 20, 23, 0, 0, 0, BeijingLocation)
			require.NoError(t, persistCycleFirstDays(context.Background(), db, store.now))

			queryAt := time.Date(2026, 9, 21, 0, 30, 0, 0, BeijingLocation).AddDate(0, 0, retentionDays)
			store.now = queryAt
			require.NoError(t, persistCycleFirstDays(context.Background(), db, queryAt))

			startDay := BeijingDay(cycleStart)
			closedDays := 0
			for ledgerDay := startDay.AddDate(0, 0, 1); ledgerDay.Before(BeijingDay(queryAt)); ledgerDay = ledgerDay.AddDate(0, 0, 1) {
				closedDays++
				require.NoError(t, db.Create(&models.TrafficDailyLedger{
					Client: client.UUID, Day: dayKey(ledgerDay), UpBytes: 1, DownBytes: 2,
				}).Error)
			}

			snapshot, err := LoadCalibrationSnapshot(context.Background(), db, client, queryAt)
			require.NoError(t, err)
			assert.Equal(t, int64(137)+int64(closedDays), snapshot.Raw.Up)
			assert.Equal(t, int64(248)+int64(closedDays)*2, snapshot.Raw.Down)

			var row models.TrafficCycleFirstDay
			require.NoError(t, db.Where("client = ?", client.UUID).Take(&row).Error)
			assert.True(t, row.Sealed)
			assert.Equal(t, int64(137), row.UpBytes)
			assert.Equal(t, int64(248), row.DownBytes)
		})
	}
}

func TestCalibrationFirstDayUsesRemainingSeriesWhenOneMetricIsDisabled(t *testing.T) {
	db := openCalibrationTestDB(t, "calibration-first-day-disabled-metric")
	day := 20
	client := models.Client{
		UUID: "node-disabled", Token: "token-disabled",
		TrafficResetDay: &day, TrafficResetTime: "00:05:00", TrafficResetTimezone: "Asia/Shanghai",
	}
	require.NoError(t, db.Create(&client).Error)
	cycleStart := time.Date(2026, 9, 20, 0, 5, 0, 0, BeijingLocation)
	s := openTrafficMetricStore(t, 1)
	_, err := s.SetMetricRetention(context.Background(), metricstore.MetricTrafficUp, 0)
	require.NoError(t, err)
	require.NoError(t, writeFirstDayTraffic(s, client.UUID, []trafficSample{
		{at: cycleStart.Add(-time.Second), upTotal: 0, downTotal: 0},
		{at: cycleStart.Add(time.Minute), upTotal: 40, downTotal: 50, downDelta: 50},
	}, false))
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, BeijingLocation)
	require.NoError(t, persistCycleFirstDays(context.Background(), db, now))
	snapshot, err := LoadCalibrationSnapshot(context.Background(), db, client, now)
	require.NoError(t, err)
	assert.Equal(t, int64(40), snapshot.Raw.Up)
	assert.Equal(t, int64(50), snapshot.Raw.Down)
}

func TestCalibrationFirstDayPersistsCompleteTotalWithOneDayRetention(t *testing.T) {
	db := openCalibrationTestDB(t, "calibration-first-day-real-store")
	day := 20
	client := models.Client{
		UUID: "node-real", Token: "token-real",
		TrafficResetDay: &day, TrafficResetTime: "00:05:00", TrafficResetTimezone: "Asia/Shanghai",
	}
	require.NoError(t, db.Create(&client).Error)
	cycleStart := time.Date(2026, 9, 20, 0, 5, 0, 0, BeijingLocation)
	s := openTrafficMetricStore(t, 1)
	samples := []trafficSample{
		{at: cycleStart.Add(-time.Second), upTotal: 0, downTotal: 0},
		{at: cycleStart.Add(time.Minute), upTotal: 100, downTotal: 200, upDelta: 100, downDelta: 200},
		{at: cycleStart.Add(25 * time.Minute), upTotal: 150, downTotal: 260, upDelta: 50, downDelta: 60},
		{at: cycleStart.Add(12 * time.Hour), upTotal: 450, downTotal: 660, upDelta: 300, downDelta: 400},
		{at: time.Date(2026, 9, 20, 23, 0, 0, 0, BeijingLocation), upTotal: 460, downTotal: 680, upDelta: 10, downDelta: 20},
	}
	require.NoError(t, writeFirstDayTraffic(s, client.UUID, samples, true))

	require.NoError(t, persistCycleFirstDays(context.Background(), db, time.Date(2026, 9, 20, 0, 30, 0, 0, BeijingLocation)))
	require.NoError(t, persistCycleFirstDays(context.Background(), db, time.Date(2026, 9, 20, 23, 0, 0, 0, BeijingLocation)))

	maintainAt := time.Date(2026, 9, 21, 0, 30, 0, 0, BeijingLocation)
	require.NoError(t, deleteTrafficBefore(s, maintainAt.Add(-24*time.Hour)))
	require.NoError(t, persistCycleFirstDays(context.Background(), db, maintainAt))

	var row models.TrafficCycleFirstDay
	require.NoError(t, db.Where("client = ?", client.UUID).Take(&row).Error)
	assert.True(t, row.Sealed)
	assert.Equal(t, int64(460), row.UpBytes)
	assert.Equal(t, int64(680), row.DownBytes)

	snapshot, err := LoadCalibrationSnapshot(context.Background(), db, client, maintainAt)
	require.NoError(t, err)
	assert.Equal(t, int64(460), snapshot.Raw.Up)
	assert.Equal(t, int64(680), snapshot.Raw.Down)
}

func TestCalibrationBackfillsShortFirstDayAfterHourlyMaintainMissesIt(t *testing.T) {
	db := openCalibrationTestDB(t, "calibration-short-first-day")
	day := 20
	client := models.Client{
		UUID: "node-short", Token: "token-short",
		TrafficResetDay: &day, TrafficResetTime: "23:45:00", TrafficResetTimezone: "Asia/Shanghai",
	}
	require.NoError(t, db.Create(&client).Error)
	cycleStart := time.Date(2026, 9, 20, 23, 45, 0, 0, BeijingLocation)
	s := openTrafficMetricStore(t, 1)
	require.NoError(t, writeFirstDayTraffic(s, client.UUID, []trafficSample{
		{at: cycleStart.Add(-time.Second), upTotal: 0, downTotal: 0},
		{at: cycleStart.Add(time.Minute), upTotal: 80, downTotal: 90, upDelta: 80, downDelta: 90},
		{at: cycleStart.Add(10 * time.Minute), upTotal: 120, downTotal: 150, upDelta: 40, downDelta: 60},
	}, true))

	maintainAt := time.Date(2026, 9, 21, 0, 30, 0, 0, BeijingLocation)
	require.NoError(t, persistCycleFirstDays(context.Background(), db, maintainAt))

	var row models.TrafficCycleFirstDay
	require.NoError(t, db.Where("client = ?", client.UUID).Take(&row).Error)
	assert.True(t, row.Sealed)
	assert.Equal(t, int64(120), row.UpBytes)
	assert.Equal(t, int64(150), row.DownBytes)

	snapshot, err := LoadCalibrationSnapshot(context.Background(), db, client, maintainAt)
	require.NoError(t, err)
	assert.True(t, snapshot.HistoryComplete)
	assert.Equal(t, int64(120), snapshot.Raw.Up)
	assert.Equal(t, int64(150), snapshot.Raw.Down)
}

func TestCalibrationDoesNotSealIncompleteFirstDayAfterUpgrade(t *testing.T) {
	db := openCalibrationTestDB(t, "calibration-first-day-unavailable")
	day := 20
	client := models.Client{
		UUID: "node-upgrade", Token: "token-upgrade",
		TrafficResetDay: &day, TrafficResetTime: "00:05:00", TrafficResetTimezone: "Asia/Shanghai",
	}
	require.NoError(t, db.Create(&client).Error)
	cycleStart := time.Date(2026, 9, 20, 0, 5, 0, 0, BeijingLocation)
	s := openTrafficMetricStore(t, 1)
	require.NoError(t, writeFirstDayTraffic(s, client.UUID, []trafficSample{
		{at: cycleStart.Add(-time.Second), upTotal: 0, downTotal: 0},
		{at: cycleStart.Add(time.Minute), upTotal: 100, downTotal: 200, upDelta: 100, downDelta: 200},
		{at: cycleStart.Add(12 * time.Hour), upTotal: 400, downTotal: 500, upDelta: 300, downDelta: 300},
	}, true))

	maintainAt := time.Date(2026, 9, 21, 0, 30, 0, 0, BeijingLocation)
	require.NoError(t, deleteTrafficBefore(s, maintainAt.Add(-24*time.Hour)))
	require.NoError(t, persistCycleFirstDays(context.Background(), db, maintainAt))

	var count int64
	require.NoError(t, db.Model(&models.TrafficCycleFirstDay{}).Where("client = ?", client.UUID).Count(&count).Error)
	assert.Equal(t, int64(0), count)

	truncated, err := MetricUsage(context.Background(), client.UUID, cycleStart.UTC(), BeijingDay(cycleStart).AddDate(0, 0, 1).Add(-time.Nanosecond).UTC())
	require.NoError(t, err)
	snapshot, err := LoadCalibrationSnapshot(context.Background(), db, client, maintainAt)
	require.NoError(t, err)
	assert.NotEqual(t, truncated.Up, snapshot.Raw.Up)
	assert.Equal(t, int64(0), snapshot.Raw.Up)
	assert.Equal(t, int64(0), snapshot.Raw.Down)
	assert.False(t, snapshot.HistoryComplete)

	cycle := snapshot.Cycle
	require.NoError(t, db.Create(&models.TrafficCalibrationAdjustment{
		CalibrationID: "old-cal", Client: client.UUID, Cycle: cycle, Day: dayKey(BeijingDay(maintainAt)),
		UpDelta: 5, DownDelta: 6, TargetUp: 5, TargetDown: 6, CreatedAt: maintainAt.UTC(),
	}).Error)
	InvalidateCalibratedCycleCache()
	usages, err := CurrentCalibratedCycleUsages(context.Background(), db, maintainAt)
	require.NoError(t, err)
	_, overlaid := usages[client.UUID]
	assert.False(t, overlaid)

	recovered, err := CalibrateCurrentCycle(context.Background(), db, client, Usage{Up: 900, Down: 800}, "admin", maintainAt)
	require.NoError(t, err)
	assert.True(t, recovered.HistoryComplete)
	assert.Equal(t, int64(900), recovered.Effective.Up)
	assert.Equal(t, int64(800), recovered.Effective.Down)

	var row models.TrafficCycleFirstDay
	require.NoError(t, db.Where("client = ? AND cycle = ?", client.UUID, cycle).Take(&row).Error)
	assert.True(t, row.Recovered)
	assert.True(t, row.Sealed)

	InvalidateCalibratedCycleCache()
	usages, err = CurrentCalibratedCycleUsages(context.Background(), db, maintainAt)
	require.NoError(t, err)
	assert.Equal(t, Usage{Up: 900, Down: 800}, usages[client.UUID])
}

type usagePoint struct {
	at       time.Time
	up, down int64
}

type retainedUsage struct {
	points    []usagePoint
	now       time.Time
	retention time.Duration
}

func (r *retainedUsage) usage(_ context.Context, _ string, start, end time.Time) (Usage, error) {
	cutoff := r.now.Add(-r.retention)
	total := Usage{}
	for _, point := range r.points {
		if point.at.Before(start) || point.at.After(end) || point.at.Before(cutoff) {
			continue
		}
		total.Up += point.up
		total.Down += point.down
	}
	return total, nil
}

type trafficSample struct {
	at                 time.Time
	upTotal, downTotal int64
	upDelta, downDelta int64
}

func openTrafficMetricStore(t *testing.T, retentionDays int) *metric.Store {
	t.Helper()
	ctx := context.Background()
	dsn := fmt.Sprintf("file:first-day-metrics-%d?mode=memory&cache=shared", time.Now().UnixNano())
	s, err := metric.Open(ctx, metric.SQLite(dsn, metric.WithMaxOpenConns(1)))
	require.NoError(t, err)
	defs := []metric.Definition{
		{Name: metricstore.MetricNetTotalUp, Type: metric.TypeCounter, Unit: "bytes", RetentionDays: retentionDays},
		{Name: metricstore.MetricNetTotalDown, Type: metric.TypeCounter, Unit: "bytes", RetentionDays: retentionDays},
		{Name: metricstore.MetricTrafficUp, Type: metric.TypeGauge, Unit: "bytes", RetentionDays: retentionDays},
		{Name: metricstore.MetricTrafficDown, Type: metric.TypeGauge, Unit: "bytes", RetentionDays: retentionDays},
	}
	for _, def := range defs {
		require.NoError(t, s.CreateMetric(ctx, def))
	}
	restore := metricstore.SwapStoreForTest(s)
	t.Cleanup(func() {
		restore()
		_ = s.Close()
	})
	return s
}

func writeFirstDayTraffic(s *metric.Store, clientID string, samples []trafficSample, includeUpDelta bool) error {
	points := make([]metric.Point, 0, len(samples)*4)
	for _, sample := range samples {
		points = append(points,
			metric.Point{MetricName: metricstore.MetricNetTotalUp, EntityID: clientID, Timestamp: sample.at.UTC(), Value: float64(sample.upTotal)},
			metric.Point{MetricName: metricstore.MetricNetTotalDown, EntityID: clientID, Timestamp: sample.at.UTC(), Value: float64(sample.downTotal)},
		)
		if includeUpDelta && sample.upDelta != 0 {
			points = append(points, metric.Point{MetricName: metricstore.MetricTrafficUp, EntityID: clientID, Timestamp: sample.at.UTC(), Value: float64(sample.upDelta)})
		}
		if sample.downDelta != 0 {
			points = append(points, metric.Point{MetricName: metricstore.MetricTrafficDown, EntityID: clientID, Timestamp: sample.at.UTC(), Value: float64(sample.downDelta)})
		}
	}
	return s.WriteBatch(context.Background(), points)
}

func deleteTrafficBefore(s *metric.Store, cutoff time.Time) error {
	ctx := context.Background()
	for _, name := range []string{
		metricstore.MetricNetTotalUp, metricstore.MetricNetTotalDown,
		metricstore.MetricTrafficUp, metricstore.MetricTrafficDown,
	} {
		if _, err := s.DeleteBefore(ctx, name, cutoff.UTC()); err != nil {
			return err
		}
	}
	return nil
}
