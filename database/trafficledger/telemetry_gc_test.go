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
	require.Equal(t, 1, n)
	var state models.TelemetryTrafficState
	require.NoError(t, db.First(&state, "client = ?", "client-a").Error)
	require.Equal(t, int64(7), state.AccountedUp)
	require.Greater(t, state.HourlyPrunedThrough, int64(0))
	var daily models.TrafficDailyLedger
	require.NoError(t, db.First(&daily, "client = ?", "client-a").Error)
	require.Equal(t, int64(7), daily.UpBytes)
	_, _, err = TelemetryUsageBatch(ctx, db, []string{"client-a"}, at, at.Add(time.Hour), time.Hour)
	require.True(t, errors.Is(err, metric.ErrTelemetryCoverage), "expired hourly history returned a false zero")
}
