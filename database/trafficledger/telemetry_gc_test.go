package trafficledger

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/pkg/metric"
	"github.com/stretchr/testify/require"
)

func TestTelemetryHourlyGCLeavesCountersAndDailyHistory(t *testing.T) {
	db := openLedgerTestDB(t, "telemetry-gc")
	require.NoError(t, db.AutoMigrate(TelemetryModels()...))
	ctx := context.Background()
	at := time.Now().UTC().Add(-40 * 24 * time.Hour).Truncate(time.Hour)
	e := metric.TelemetryTrafficEvent{StoreID: "gc-store", Source: "client-a", IngestID: 1, Observed: at, Received: at, Traffic: metric.TelemetryTraffic{TotalUp: 100, TotalDown: 200}}
	require.NoError(t, ApplyTelemetry(ctx, db, e))
	e.IngestID = 2
	e.Observed = at.Add(time.Second)
	e.Traffic.TotalUp += 7
	require.NoError(t, ApplyTelemetry(ctx, db, e))
	prior := telemetryMetadata.Load()
	ConfigureTelemetry(db)
	defer telemetryMetadata.Store(prior)
	n, err := PruneTelemetrySlices(ctx, time.Now(), 128)
	require.NoError(t, err)
	require.Equal(t, 4, n) // One hour, one minute, and two accepted edge events.
	var state models.TelemetryTrafficState
	require.NoError(t, db.First(&state, "client = ?", "client-a").Error)
	require.Equal(t, int64(7), state.AccountedUp)
	require.Greater(t, state.HourlyPrunedThrough, int64(0))
	require.Greater(t, state.MinutePrunedThrough, int64(0))
	require.Greater(t, state.TailPrunedThrough, int64(0))
	var daily models.TrafficDailyLedger
	require.NoError(t, db.First(&daily, "client = ?", "client-a").Error)
	require.Equal(t, int64(7), daily.UpBytes)
	_, _, err = TelemetryUsageBatch(ctx, db, []string{"client-a"}, at, at.Add(time.Hour), time.Hour)
	require.True(t, errors.Is(err, metric.ErrTelemetryCoverage), "expired hourly history returned a false zero")
}

func TestTelemetryGCBudgetAndAtomicWatermarks(t *testing.T) {
	db := openLedgerTestDB(t, "telemetry-gc-budget")
	require.NoError(t, db.AutoMigrate(TelemetryModels()...))
	prior := telemetryMetadata.Load()
	ConfigureTelemetry(db)
	defer telemetryMetadata.Store(prior)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Minute)
	require.NoError(t, db.Create(&models.TelemetryTrafficState{Client: "a"}).Error)
	for i := 0; i < 8; i++ {
		at := now.Add(-40*24*time.Hour + time.Duration(i)*time.Minute).UnixNano()
		require.NoError(t, db.Create(&models.TelemetryTrafficSlice{Client: "a", Start: at, End: at + int64(time.Minute)}).Error)
		require.NoError(t, db.Create(&models.TelemetryTrafficMinute{Client: "a", Start: at}).Error)
		require.NoError(t, db.Create(&models.TelemetryTrafficTail{Client: "a", Observed: at}).Error)
	}
	require.NoError(t, db.Exec(`CREATE TRIGGER reject_tail_prune BEFORE DELETE ON telemetry_traffic_tails BEGIN SELECT RAISE(FAIL, 'gc rejected'); END`).Error)
	n, err := PruneTelemetrySlices(ctx, now, 24)
	require.Error(t, err)
	require.Zero(t, n)
	var state models.TelemetryTrafficState
	require.NoError(t, db.First(&state).Error)
	require.Zero(t, state.MinutePrunedThrough)
	require.Zero(t, state.HourlyPrunedThrough)
	require.Zero(t, state.TailPrunedThrough)
	for _, model := range []any{&models.TelemetryTrafficSlice{}, &models.TelemetryTrafficMinute{}, &models.TelemetryTrafficTail{}} {
		var count int64
		require.NoError(t, db.Model(model).Count(&count).Error)
		require.Equal(t, int64(8), count)
	}
	require.NoError(t, db.Exec("DROP TRIGGER reject_tail_prune").Error)
	for remaining := 24; remaining > 0; remaining -= 3 {
		n, err = PruneTelemetrySlices(ctx, now, 3)
		require.NoError(t, err)
		require.Equal(t, 3, n, "one shared budget across all tables")
	}
	n, err = PruneTelemetrySlices(ctx, now, 3)
	require.NoError(t, err)
	require.Zero(t, n)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = PruneTelemetrySlices(cancelled, now, 3)
	require.ErrorIs(t, err, context.Canceled)
}
