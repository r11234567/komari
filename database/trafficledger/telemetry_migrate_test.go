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

func TestTelemetryOfflineLedgerAndFirstOnlineDelta(t *testing.T) {
	db := openLedgerTestDB(t, "telemetry-offline-ledger")
	require.NoError(t, db.AutoMigrate(TelemetryModels()...))
	config.SetDb(db)
	dir := t.TempDir()
	ctx := context.Background()
	archive := filepath.Join(dir, "archive")
	require.NoError(t, config.Set("metric_archive_directory", archive))
	s, err := metric.Open(ctx, metric.SQLite(filepath.Join(dir, "metrics.db"), metric.WithRetiredLegacyStorage(), metric.WithSQLiteProfile(metric.SQLiteProfileDurable)))
	require.NoError(t, err)
	defer s.Close()
	require.NoError(t, metricstore.EnsureBuiltinMetricDefinitions(ctx, s))
	restore := metricstore.SwapStoreForTest(s)
	defer restore()
	now := time.Now().UTC()
	today := BeijingDay(now)
	at := maxTimeLedger(today, now.Add(-20*time.Minute))
	var points []metric.Point
	for i, counters := range [][2]float64{{100, 200}, {130, 250}} {
		when := at.Add(time.Duration(i) * time.Second)
		for name, value := range map[string]float64{metricstore.MetricNetTotalUp: counters[0], metricstore.MetricNetTotalDown: counters[1], metricstore.MetricTrafficUp: float64(i * 30), metricstore.MetricTrafficDown: float64(i * 50)} {
			points = append(points, metric.Point{MetricName: name, EntityID: "client-a", Timestamp: when, Value: value})
		}
	}
	require.NoError(t, s.WriteBatch(ctx, points))
	want, err := MetricUsage(ctx, "client-a", today, now)
	require.NoError(t, err)
	require.NoError(t, MigrateTelemetryLedger(ctx, db, now))
	// A second invocation replaces, rather than increments, offline rows.
	require.NoError(t, MigrateTelemetryLedger(ctx, db, now))
	require.NoError(t, s.MigrateLegacy(ctx, metric.TelemetryConfig{ArchiveDirectory: archive}, nil))
	require.NoError(t, s.EnableTelemetry(ctx, metric.TelemetryConfig{ArchiveDirectory: archive}))
	prior := telemetryMetadata.Load()
	ConfigureTelemetry(db)
	defer telemetryMetadata.Store(prior)
	got, err := MetricUsage(ctx, "client-a", today, now)
	require.NoError(t, err)
	require.Equal(t, want, got)
	report := v1.Report{UUID: "client-a", UpdatedAt: now.Add(time.Second)}
	report.Network.TotalUp = 137
	report.Network.TotalDown = 261
	_, err = metricstore.WriteReport(metricstore.WithReportIdentity(ctx, "online/1", ""), report)
	require.NoError(t, err)
	// Delivery is background-owned. Reads must not secretly consume the outbox.
	n, err := metricstore.DeliverTelemetryTraffic(ctx, 128, ConsumeTelemetry)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	got, err = MetricUsage(ctx, "client-a", today, report.UpdatedAt)
	require.NoError(t, err)
	require.Equal(t, Usage{Up: want.Up + 7, Down: want.Down + 11}, got)
	rollbackPath := filepath.Join(dir, "rollback.db")
	require.NoError(t, s.Telemetry().RollbackLegacy(ctx, rollbackPath, nil))
	legacy, err := metric.Open(ctx, metric.SQLite(rollbackPath, metric.WithRetiredLegacyStorage()))
	require.NoError(t, err)
	useTelemetry := metricstore.SwapStoreForTest(legacy)
	rolledBack, rollbackErr := MetricUsage(ctx, "client-a", today, report.UpdatedAt)
	useTelemetry()
	legacy.Close()
	require.NoError(t, rollbackErr)
	require.Equal(t, got, rolledBack, "rollback changed traffic accounting")
	// Raw ownership can disappear without changing the business result.
	_, err = s.Telemetry().EraseSeries(ctx, metric.Query{EntityID: "client-a"})
	require.NoError(t, err)
	again, err := MetricUsage(ctx, "client-a", today, report.UpdatedAt)
	require.NoError(t, err)
	require.Equal(t, got, again)
}

func TestOfflineMinuteRefinementReconcilesAndKeepsPartialHistoryCoarse(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(fmt.Sprint(partial), func(t *testing.T) {
			db := openLedgerTestDB(t, fmt.Sprintf("offline-minute-refinement-%v", partial))
			require.NoError(t, db.AutoMigrate(TelemetryModels()...))
			config.SetDb(db)
			ctx := context.Background()
			dir := t.TempDir()
			require.NoError(t, config.Set("metric_archive_directory", filepath.Join(dir, "archive")))
			s, err := metric.Open(ctx, metric.SQLite(filepath.Join(dir, "metrics.db"), metric.WithRetiredLegacyStorage(), metric.WithSQLiteProfile(metric.SQLiteProfileDurable)))
			require.NoError(t, err)
			defer s.Close()
			require.NoError(t, metricstore.EnsureBuiltinMetricDefinitions(ctx, s))
			restore := metricstore.SwapStoreForTest(s)
			defer restore()
			at := time.Now().UTC().Add(-time.Hour).Truncate(time.Hour)
			now := at.Add(55 * time.Minute)
			var points []metric.Point
			for i := 0; i < 3; i++ {
				when := at.Add(time.Duration(i)*20*time.Minute + time.Second)
				for name, value := range map[string]float64{metricstore.MetricNetTotalUp: float64(1000 + i*100), metricstore.MetricNetTotalDown: float64(2000 + i*200), metricstore.MetricTrafficUp: float64(min(i, 1) * 100), metricstore.MetricTrafficDown: float64(min(i, 1) * 200)} {
					if partial && i == 1 && name == metricstore.MetricTrafficUp {
						continue
					}
					points = append(points, metric.Point{MetricName: name, EntityID: "client-a", Timestamp: when, Value: value})
				}
			}
			require.NoError(t, s.WriteBatch(ctx, points))
			want, err := MetricUsage(ctx, "client-a", at, now)
			require.NoError(t, err)
			for i := 0; i < 2; i++ {
				require.NoError(t, MigrateTelemetryLedger(ctx, db, now))
			}
			var state models.TelemetryTrafficState
			require.NoError(t, db.Where("client=?", "client-a").Take(&state).Error)
			if partial {
				require.Zero(t, state.MinuteSince)
				return
			}
			require.Equal(t, at.UnixNano(), state.MinuteSince)
			totals, groups, err := TelemetryUsageBatch(ctx, db, []string{"client-a"}, at, now, 20*time.Minute)
			require.NoError(t, err)
			require.Equal(t, want, totals["client-a"])
			require.Len(t, groups["client-a"], 3)
			require.Equal(t, int64(100), groups["client-a"][1].Usage.Up)
			var day models.TrafficDailyLedger
			require.NoError(t, db.Where("client=? AND day=?", "client-a", dayKey(now)).Take(&day).Error)
			require.Equal(t, want.Up, day.UpBytes)
			require.Equal(t, want.Down, day.DownBytes)
		})
	}
}

func maxTimeLedger(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
