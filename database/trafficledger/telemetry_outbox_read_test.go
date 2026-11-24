package trafficledger

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/komari-monitor/komari/database/metricstore"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/pkg/metric"
	v1 "github.com/komari-monitor/komari/protocol/v1"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func openCalibrationOutbox(t *testing.T, name string) *gorm.DB {
	t.Helper()
	ctx := context.Background()
	db := openLedgerTestDB(t, name)
	metadata, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, metadata.Close()) })
	require.NoError(t, db.AutoMigrate(TelemetryModels()...))
	dir := t.TempDir()
	archive := filepath.Join(dir, "archive")
	s, err := metric.Open(ctx, metric.SQLite(filepath.Join(dir, "metrics.db"), metric.WithSQLiteProfile(metric.SQLiteProfileDurable)))
	require.NoError(t, err)
	t.Cleanup(func() { s.Close() })
	require.NoError(t, metricstore.EnsureBuiltinMetricDefinitions(ctx, s))
	require.NoError(t, s.EnableTelemetry(ctx, metric.TelemetryConfig{ArchiveDirectory: archive}))
	t.Cleanup(metricstore.SwapStoreForTest(s))
	prior := telemetryMetadata.Load()
	ConfigureTelemetry(db)
	t.Cleanup(func() { telemetryMetadata.Store(prior) })
	return db
}

func TestTelemetryForegroundDoesNotDrain128Or129Reports(t *testing.T) {
	for _, count := range []int{128, 129} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			ctx := context.Background()
			db := openCalibrationOutbox(t, fmt.Sprintf("outbox-read-%d", count))
			at := time.Now().UTC().Add(-3 * time.Minute)
			for i := 0; i < count; i++ {
				report := v1.Report{UUID: "client-a", UpdatedAt: at.Add(time.Duration(i) * time.Second)}
				report.Network.TotalUp = int64(100 + i)
				report.Network.TotalDown = int64(200 + 2*i)
				report.Network.Up = 1
				report.Network.Down = 2
				_, err := metricstore.WriteReport(metricstore.WithReportIdentity(ctx, fmt.Sprintf("pending-%d", i)), report)
				require.NoError(t, err)
			}
			pending, err := metricstore.TelemetryTrafficPending(ctx)
			require.NoError(t, err)
			require.True(t, pending)
			client := models.Client{UUID: "client-a", TrafficResetDay: intPointer(1), TrafficLimitType: "sum"}
			_, err = CalibrateCurrentCycle(ctx, db, client, Usage{Up: 1, Down: 2}, "test", time.Now().UTC())
			require.ErrorIs(t, err, ErrCalibrationCatchingUp)
			_, err = CalibrateBillableCycle(ctx, db, client, 3, "test", time.Now().UTC())
			require.ErrorIs(t, err, ErrCalibrationCatchingUp)
			got, err := MetricUsage(ctx, "client-a", at.Add(-time.Minute), time.Now().UTC())
			require.NoError(t, err)
			require.Equal(t, Usage{}, got)
			var consumed int64
			require.NoError(t, db.Model(&struct{ StoreID string }{}).Table("telemetry_traffic_streams").Count(&consumed).Error)
			require.Zero(t, consumed, "foreground read claimed outbox")
			n, err := metricstore.DeliverTelemetryTraffic(ctx, 128, ConsumeTelemetry)
			require.NoError(t, err)
			require.Equal(t, 128, n)
			got, err = MetricUsage(ctx, "client-a", at.Add(-time.Minute), time.Now().UTC())
			require.NoError(t, err)
			require.Equal(t, Usage{Up: 127, Down: 254}, got, "exactly 128 deliveries must not fail a read")
			pending, err = metricstore.TelemetryTrafficPending(ctx)
			require.NoError(t, err)
			require.Equal(t, count == 129, pending)
			n, err = metricstore.DeliverTelemetryTraffic(ctx, 128, ConsumeTelemetry)
			require.NoError(t, err)
			require.Equal(t, count-128, n)
			got, err = MetricUsage(ctx, "client-a", at.Add(-time.Minute), time.Now().UTC())
			require.NoError(t, err)
			require.Equal(t, int64(count-1), got.Up)
			snapshot, err := CalibrateCurrentCycle(ctx, db, client, Usage{Up: 1, Down: 2}, "test", time.Now().UTC())
			require.NoError(t, err)
			require.Equal(t, Usage{Up: 1, Down: 2}, snapshot.Effective)
		})
	}
}

func TestCalibrationOnlyWaitsForItsOwnTraffic(t *testing.T) {
	ctx := context.Background()
	db := openCalibrationOutbox(t, "calibration-source")
	client := models.Client{UUID: "client-b", Token: "token-b", TrafficResetDay: intPointer(1), TrafficLimitType: "sum"}
	require.NoError(t, db.Create(&client).Error)
	at := time.Now().UTC().Truncate(time.Minute).Add(-time.Minute)
	now := at.Add(30 * time.Second)
	write := func(source, id string, offset time.Duration, up, down int64) {
		t.Helper()
		report := v1.Report{UUID: source, UpdatedAt: at.Add(offset)}
		report.Network.TotalUp, report.Network.TotalDown = up, down
		report.Network.Up, report.Network.Down = 1, 2
		_, err := metricstore.WriteReport(metricstore.WithReportIdentity(ctx, id), report)
		require.NoError(t, err)
	}
	write(client.UUID, "baseline", 0, 100, 200)
	n, err := metricstore.DeliverTelemetryTraffic(ctx, 8, ConsumeTelemetry)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	write("client-a", "other", time.Second, 100, 200)

	snapshot, err := CalibrateCurrentCycle(ctx, db, client, Usage{Up: 4, Down: 8}, "test", now)
	require.NoError(t, err)
	require.Equal(t, Usage{Up: 4, Down: 8}, snapshot.Effective)
	snapshot, err = CalibrateBillableCycle(ctx, db, client, 18, "test", now.Add(time.Second))
	require.NoError(t, err)
	require.Equal(t, Usage{Up: 6, Down: 12}, snapshot.Effective)
	pending, err := metricstore.TelemetryTrafficPending(ctx)
	require.NoError(t, err)
	require.True(t, pending, "calibration must leave the other node's outbox pending")

	write(client.UUID, "growth", 2*time.Second, 101, 202)
	_, err = CalibrateCurrentCycle(ctx, db, client, Usage{Up: 8, Down: 16}, "test", now)
	require.ErrorIs(t, err, ErrCalibrationCatchingUp)
	_, err = CalibrateBillableCycle(ctx, db, client, 24, "test", now)
	require.ErrorIs(t, err, ErrCalibrationCatchingUp)
	n, err = metricstore.DeliverTelemetryTraffic(ctx, 8, ConsumeTelemetry)
	require.NoError(t, err)
	require.Equal(t, 2, n)
	n, err = metricstore.DeliverTelemetryTraffic(ctx, 8, ConsumeTelemetry)
	require.NoError(t, err)
	require.Zero(t, n)
	snapshot, err = LoadCalibrationSnapshot(ctx, db, client, now)
	require.NoError(t, err)
	require.Equal(t, Usage{Up: 1, Down: 2}, snapshot.Raw)
	require.Equal(t, Usage{Up: 7, Down: 14}, snapshot.Effective)
}
