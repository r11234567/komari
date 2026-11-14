package trafficledger

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/komari-monitor/komari/database/metricstore"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/pkg/config"
	"github.com/komari-monitor/komari/pkg/metric"
	v1 "github.com/komari-monitor/komari/protocol/v1"
	"github.com/stretchr/testify/require"
)

func TestTelemetryForegroundDoesNotDrain128Or129Reports(t *testing.T) {
	for _, count := range []int{128, 129} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			ctx := context.Background()
			db := openLedgerTestDB(t, fmt.Sprintf("outbox-read-%d", count))
			require.NoError(t, db.AutoMigrate(TelemetryModels()...))
			config.SetDb(db)
			dir := t.TempDir()
			archive := filepath.Join(dir, "archive")
			require.NoError(t, config.Set("metric_archive_directory", archive))
			s, err := metric.Open(ctx, metric.SQLite(filepath.Join(dir, "metrics.db"), metric.WithSQLiteProfile(metric.SQLiteProfileDurable)))
			require.NoError(t, err)
			defer s.Close()
			require.NoError(t, metricstore.EnsureBuiltinMetricDefinitions(ctx, s))
			require.NoError(t, s.EnableTelemetry(ctx, metric.TelemetryConfig{ArchiveDirectory: archive}))
			restore := metricstore.SwapStoreForTest(s)
			defer restore()
			prior := telemetryMetadata.Load()
			ConfigureTelemetry(db)
			defer telemetryMetadata.Store(prior)
			at := time.Now().UTC().Add(-3 * time.Minute)
			for i := 0; i < count; i++ {
				report := v1.Report{UUID: "client-a", UpdatedAt: at.Add(time.Duration(i) * time.Second)}
				report.Network.TotalUp = int64(100 + i)
				report.Network.TotalDown = int64(200 + 2*i)
				report.Network.Up = 1
				report.Network.Down = 2
				_, err = metricstore.WriteReport(metricstore.WithReportIdentity(ctx, fmt.Sprintf("pending-%d", i)), report)
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
