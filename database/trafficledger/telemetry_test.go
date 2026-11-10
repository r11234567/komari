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

func TestTelemetryTrafficIdempotentIntegerCountersAndCalendar(t *testing.T) {
	db := openLedgerTestDB(t, "telemetry-integer-ledger")
	require.NoError(t, db.AutoMigrate(TelemetryModels()...))
	ctx := context.Background()
	at := time.Date(2026, 11, 1, 23, 59, 50, 0, BeijingLocation)
	base := int64(1 << 54)
	event := metric.TelemetryTrafficEvent{StoreID: "store-one", Source: "client-a", IngestID: 1, Observed: at, Received: at, Traffic: metric.TelemetryTraffic{TotalUp: base, TotalDown: base, Up: 100, Down: 100}}
	require.NoError(t, ApplyTelemetry(ctx, db, event))
	event.IngestID = 2
	event.Observed = at.Add(5 * time.Second)
	event.Traffic.TotalUp += 1
	event.Traffic.TotalDown += 3
	require.NoError(t, ApplyTelemetry(ctx, db, event))
	require.NoError(t, ApplyTelemetry(ctx, db, event))
	event.IngestID = 3
	event.Observed = at.Add(15 * time.Second)
	event.Traffic.TotalUp += 2
	event.Traffic.TotalDown += 5
	require.NoError(t, ApplyTelemetry(ctx, db, event))
	var state models.TelemetryTrafficState
	require.NoError(t, db.First(&state, "client = ?", "client-a").Error)
	require.Equal(t, base+3, state.TotalUp)
	require.Equal(t, int64(3), state.AccountedUp)
	require.Equal(t, int64(8), state.AccountedDown)
	usage, hourly, err := TelemetryUsageBatch(ctx, db, []string{"client-a"}, at.Add(-time.Hour), at.Add(time.Hour), time.Hour)
	require.NoError(t, err)
	require.Equal(t, Usage{Up: 3, Down: 8}, usage["client-a"])
	require.Len(t, hourly["client-a"], 2)
	var days []models.TrafficDailyLedger
	require.NoError(t, db.Order("day").Find(&days).Error)
	require.Len(t, days, 2)
	require.Equal(t, int64(1), days[0].UpBytes)
	require.Equal(t, int64(2), days[1].UpBytes)
	// A restarted counter starts a new epoch, not an enormous negative delta.
	event.IngestID = 4
	event.Observed = at.Add(20 * time.Second)
	event.Traffic.TotalUp = 10
	event.Traffic.TotalDown = 20
	require.NoError(t, ApplyTelemetry(ctx, db, event))
	event.IngestID = 5
	event.Observed = at.Add(25 * time.Second)
	event.Traffic.TotalUp = 17
	event.Traffic.TotalDown = 29
	require.NoError(t, ApplyTelemetry(ctx, db, event))
	event.IngestID = 6
	event.Observed = at.Add(21 * time.Second)
	event.Traffic.TotalUp = 1000
	require.NoError(t, ApplyTelemetry(ctx, db, event))
	require.NoError(t, db.First(&state, "client = ?", "client-a").Error)
	require.Equal(t, int64(10), state.AccountedUp)
	require.Equal(t, int64(1), state.LateReports)
	require.Equal(t, int64(17), state.TotalUp)
}

func TestTelemetryTrafficRollbackAndExactResetBoundary(t *testing.T) {
	db := openLedgerTestDB(t, "telemetry-reset-ledger")
	require.NoError(t, db.AutoMigrate(TelemetryModels()...))
	ctx := context.Background()
	day := 1
	require.NoError(t, db.Model(&models.Client{}).Where("uuid = ?", "client-a").Updates(map[string]any{"traffic_reset_day": day, "traffic_reset_time": "00:30", "traffic_reset_timezone": "Asia/Shanghai"}).Error)
	at := time.Date(2026, 11, 1, 0, 29, 50, 0, BeijingLocation)
	e := metric.TelemetryTrafficEvent{StoreID: "store-two", Source: "client-a", IngestID: 1, Observed: at, Received: at, Traffic: metric.TelemetryTraffic{TotalUp: 100, TotalDown: 100, Up: 100, Down: 100}}
	require.NoError(t, ApplyTelemetry(ctx, db, e))
	e.IngestID = 2
	e.Observed = at.Add(5 * time.Second)
	e.Traffic.TotalUp = 110
	e.Traffic.TotalDown = 120
	require.NoError(t, ApplyTelemetry(ctx, db, e))
	// Fail a real metadata write after the outbox watermark was claimed.
	require.NoError(t, db.Exec("CREATE TRIGGER reject_traffic_slice BEFORE UPDATE ON telemetry_traffic_slices BEGIN SELECT RAISE(ABORT, 'injected metadata failure'); END").Error)
	e.IngestID = 3
	e.Observed = at.Add(7 * time.Second)
	e.Traffic.TotalUp = 115
	require.Error(t, ApplyTelemetry(ctx, db, e))
	var stream models.TelemetryTrafficStream
	require.NoError(t, db.First(&stream, "store_id = ?", e.StoreID).Error)
	require.Equal(t, int64(2), stream.Through)
	require.NoError(t, db.Exec("DROP TRIGGER reject_traffic_slice").Error)
	require.NoError(t, ApplyTelemetry(ctx, db, e))
	e.IngestID = 4
	e.Observed = at.Add(15 * time.Second)
	e.Traffic.TotalUp = 125
	e.Traffic.TotalDown = 140
	require.NoError(t, ApplyTelemetry(ctx, db, e))
	boundary := time.Date(2026, 11, 1, 0, 30, 0, 0, BeijingLocation)
	u, _, err := TelemetryUsageBatch(ctx, db, []string{"client-a"}, boundary, at.Add(time.Minute), time.Hour)
	require.NoError(t, err)
	require.Equal(t, Usage{Up: 10, Down: 20}, u["client-a"])
	_, _, err = TelemetryUsageBatch(ctx, db, []string{"client-a"}, at.Add(time.Second), at.Add(6*time.Second), time.Hour)
	require.True(t, errors.Is(err, metric.ErrTelemetryCoverage))
	var first models.TrafficCycleFirstDay
	require.NoError(t, db.First(&first, "client = ? AND day = ?", "client-a", "2026-11-01").Error)
	require.Equal(t, int64(10), first.UpBytes)
}
