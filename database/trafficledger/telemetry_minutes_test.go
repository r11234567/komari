package trafficledger

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/pkg/metric"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"gorm.io/gorm/schema"
)

// Business metadata is SQLite in production, including when the metric engine
// uses an external SQL driver. Use real WAL/physical connections, not a mock.
// External metric outbox coverage remains in the telemetry-storage driver CI.
func trafficBackends(t *testing.T, run func(*testing.T, *gorm.DB)) {
	t.Helper()
	for _, driver := range []string{"sqlite"} {
		t.Run(driver, func(t *testing.T) {
			dialector := sqlite.Open(filepath.Join(t.TempDir(), "ledger.db") + "?_journal_mode=WAL&_busy_timeout=5000&_txlock=immediate")
			db, err := gorm.Open(dialector, &gorm.Config{Logger: logger.Default.LogMode(logger.Silent), NamingStrategy: schema.NamingStrategy{TablePrefix: fmt.Sprintf("tm_%x_", time.Now().UnixNano())}})
			require.NoError(t, err)
			all := append([]any{&models.Client{}, &models.TrafficDailyLedger{}, &models.TrafficCalibrationAdjustment{}, &models.TrafficCycleFirstDay{}}, TelemetryModels()...)
			require.NoError(t, db.AutoMigrate(all...))
			sqlDB, err := db.DB()
			require.NoError(t, err)
			sqlDB.SetMaxOpenConns(4)
			t.Cleanup(func() {
				for i := len(all) - 1; i >= 0; i-- {
					require.NoError(t, db.Migrator().DropTable(all[i]))
				}
				require.NoError(t, sqlDB.Close())
			})
			require.NoError(t, db.Create(&models.Client{UUID: "client-a", Token: "token-a"}).Error)
			run(t, db)
		})
	}
}

func minuteEvent(db *gorm.DB, id int64, client string, at time.Time, delta int64) error {
	return ApplyTelemetry(context.Background(), db, metric.TelemetryTrafficEvent{StoreID: "minute-contract", IngestID: id, Source: client, Observed: at, Received: at, Traffic: metric.TelemetryTraffic{HasDelta: true, DeltaUp: delta, DeltaDown: delta * 2}})
}

func TestTelemetryMinuteRealDriverIntervalsAndFixedEndpoints(t *testing.T) {
	trafficBackends(t, func(t *testing.T, db *gorm.DB) {
		at := time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)
		for i := 0; i < 180; i++ {
			require.NoError(t, minuteEvent(db, int64(i+1), "client-a", at.Add(time.Duration(i)*20*time.Second), int64(i+1)))
		}
		for _, interval := range []time.Duration{10 * time.Minute, 20 * time.Minute, 30 * time.Minute} {
			for _, bounds := range [][2]time.Duration{{0, time.Hour - 1}, {5 * time.Minute, 45 * time.Minute}, {25 * time.Minute, 55 * time.Minute}, {25*time.Minute + time.Nanosecond, 45 * time.Minute}, {time.Second, 40 * time.Second}, {20 * time.Second, 20 * time.Second}} {
				start, end := at.Add(bounds[0]), at.Add(bounds[1])
				expected := map[int64]Usage{}
				var total Usage
				for i := 0; i < 180; i++ {
					ts := at.Add(time.Duration(i) * 20 * time.Second)
					if ts.Before(start) || ts.After(end) {
						continue
					}
					v := Usage{Up: int64(i + 1), Down: 2 * int64(i+1)}
					key := ts.Truncate(interval).UnixNano()
					expected[key] = addUsageSaturated(expected[key], v)
					total = addUsageSaturated(total, v)
				}
				got, groups, err := TelemetryUsageBatch(t.Context(), db, []string{"client-a", "client-a"}, start, end, interval)
				require.NoError(t, err, "interval=%s bounds=%v", interval, bounds)
				require.Equal(t, total, got["client-a"])
				require.Len(t, groups["client-a"], len(expected))
				for _, group := range groups["client-a"] {
					require.Equal(t, expected[group.Hour.UnixNano()], group.Usage)
				}
			}
		}
	})
}

func TestTelemetryMinuteRealDriverSnapshotAndRollback(t *testing.T) {
	trafficBackends(t, func(t *testing.T, db *gorm.DB) {
		at := time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)
		require.NoError(t, minuteEvent(db, 1, "client-a", at, 10))
		// Capture state, commit an update through a second physical connection,
		// then read mutable minutes in the original repeatable snapshot.
		require.NoError(t, WithTelemetryReadSnapshot(t.Context(), db, func(tx *gorm.DB) error {
			var state models.TelemetryTrafficState
			require.NoError(t, tx.First(&state).Error)
			done := make(chan error, 1)
			go func() { done <- minuteEvent(db, 2, "client-a", at.Add(time.Second), 20) }()
			select {
			case err := <-done:
				require.NoError(t, err)
			case <-time.After(8 * time.Second):
				t.Fatal("writer blocked behind snapshot")
			}
			var minute models.TelemetryTrafficMinute
			require.NoError(t, tx.First(&minute).Error)
			require.Equal(t, int64(10), minute.UpBytes)
			require.Equal(t, int64(10), state.AccountedUp)
			return nil
		}))
		// Failure after all other ledger mutations must roll back every layer.
		require.NoError(t, db.Callback().Create().Before("gorm:create").Register("fail-minute-tail", func(tx *gorm.DB) {
			if tx.Statement.Schema != nil && tx.Statement.Schema.Name == "TelemetryTrafficTail" {
				tx.AddError(errors.New("injected tail failure"))
			}
		}))
		require.Error(t, minuteEvent(db, 3, "client-a", at.Add(2*time.Second), 40))
		require.NoError(t, db.Callback().Create().Remove("fail-minute-tail"))
		var state models.TelemetryTrafficState
		var stream models.TelemetryTrafficStream
		var minute models.TelemetryTrafficMinute
		require.NoError(t, db.First(&state).Error)
		require.NoError(t, db.First(&stream).Error)
		require.NoError(t, db.First(&minute).Error)
		require.Equal(t, int64(30), state.AccountedUp)
		require.Equal(t, int64(30), minute.UpBytes)
		require.Equal(t, int64(2), stream.Through)
		require.NoError(t, minuteEvent(db, 3, "client-a", at.Add(2*time.Second), 40))
		require.NoError(t, minuteEvent(db, 3, "client-a", at.Add(2*time.Second), 40))
		got, _, err := TelemetryUsageBatch(t.Context(), db, []string{"client-a"}, at, at.Add(time.Minute), time.Minute)
		require.NoError(t, err)
		require.Equal(t, Usage{Up: 70, Down: 140}, got["client-a"])
	})
}

func TestTelemetryMinuteRealDriverBoundedBatchAndNoReadWrites(t *testing.T) {
	trafficBackends(t, func(t *testing.T, db *gorm.DB) {
		at := time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)
		ids := make([]string, 300)
		for i := range ids {
			ids[i] = fmt.Sprintf("node-%03d", i)
			require.NoError(t, db.Create(&models.Client{UUID: ids[i], Token: ids[i]}).Error)
			require.NoError(t, minuteEvent(db, int64(i+1), ids[i], at, int64(i+1)))
		}
		fail := func(tx *gorm.DB) { tx.AddError(errors.New("read attempted a write")) }
		require.NoError(t, db.Callback().Create().Before("gorm:create").Register("readonly", fail))
		require.NoError(t, db.Callback().Update().Before("gorm:update").Register("readonly", fail))
		require.NoError(t, db.Callback().Delete().Before("gorm:delete").Register("readonly", fail))
		got, groups, err := TelemetryUsageBatch(t.Context(), db, append(ids, ids[0]), at, at.Add(time.Minute), 20*time.Minute)
		require.NoError(t, err)
		require.Len(t, got, 300)
		require.Len(t, groups, 300)
		for i, id := range ids {
			require.Equal(t, int64(i+1), got[id].Up)
		}
		cancelled, cancel := context.WithCancel(t.Context())
		cancel()
		_, _, err = TelemetryUsageBatch(cancelled, db, ids, at, at.Add(time.Minute), time.Minute)
		require.ErrorIs(t, err, context.Canceled)
		require.NoError(t, db.Callback().Create().Remove("readonly"))
		require.NoError(t, db.Callback().Update().Remove("readonly"))
		require.NoError(t, db.Callback().Delete().Remove("readonly"))
	})
}

func TestTelemetryMinuteLegacyCoverageAndPrunedEdges(t *testing.T) {
	db := openLedgerTestDB(t, "minute-legacy-coverage")
	require.NoError(t, db.AutoMigrate(TelemetryModels()...))
	at := time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)
	require.NoError(t, minuteEvent(db, 1, "client-a", at.Add(5*time.Second), 10))
	require.NoError(t, minuteEvent(db, 2, "client-a", at.Add(25*time.Second), 20))
	// Tail loss is explicit: never substitute a complete minute for its edge.
	require.NoError(t, db.Where("client = ?", "client-a").Delete(&models.TelemetryTrafficTail{}).Error)
	require.NoError(t, db.Model(&models.TelemetryTrafficState{}).Where("client = ?", "client-a").UpdateColumn("tail_pruned_through", at.Add(time.Minute).UnixNano()).Error)
	_, _, err := TelemetryUsageBatch(t.Context(), db, []string{"client-a"}, at.Add(10*time.Second), at.Add(time.Minute), 20*time.Minute)
	require.ErrorIs(t, err, metric.ErrTelemetryCoverage)
	// Whole-minute queries still work without any event history.
	u, _, err := TelemetryUsageBatch(t.Context(), db, []string{"client-a"}, at, at.Add(time.Minute).Add(-time.Nanosecond), 20*time.Minute)
	require.NoError(t, err)
	require.Equal(t, int64(30), u["client-a"].Up)
	// Simulate an upgraded state that only ever stored hourly history.
	require.NoError(t, db.Model(&models.TelemetryTrafficState{}).Where("client = ?", "client-a").UpdateColumn("minute_since", 0).Error)
	_, _, err = TelemetryUsageBatch(t.Context(), db, []string{"client-a"}, at, at.Add(time.Hour), 20*time.Minute)
	require.ErrorIs(t, err, metric.ErrTelemetryCoverage)
	u, _, err = TelemetryUsageBatch(t.Context(), db, []string{"client-a"}, at, at.Add(time.Hour).Add(-time.Nanosecond), time.Hour)
	require.NoError(t, err)
	require.Equal(t, int64(30), u["client-a"].Up)
	_, _, err = TelemetryUsageBatch(t.Context(), db, []string{"client-a"}, at.Add(10*time.Second), at.Add(time.Hour), time.Hour)
	require.ErrorIs(t, err, metric.ErrTelemetryCoverage)
}

func TestTelemetryReadSnapshotCancellationRestoresPooledConnection(t *testing.T) {
	trafficBackends(t, func(t *testing.T, db *gorm.DB) {
		pool, err := db.DB()
		require.NoError(t, err)
		pool.SetMaxOpenConns(1)
		ctx, cancel := context.WithCancel(t.Context())
		err = WithTelemetryReadSnapshot(ctx, db, func(tx *gorm.DB) error {
			var count int64
			if err := tx.Model(&models.Client{}).Count(&count).Error; err != nil {
				return err
			}
			cancel()
			return ctx.Err()
		})
		require.ErrorIs(t, err, context.Canceled)
		// A cancelled reader must not leave BEGIN or query_only on the only
		// reusable connection. The next normal write must actually commit.
		require.NoError(t, db.Create(&models.Client{UUID: "after-cancel", Token: "after-cancel"}).Error)
		require.Error(t, WithTelemetryReadSnapshot(t.Context(), db, func(tx *gorm.DB) error {
			return tx.Create(&models.Client{UUID: "forbidden", Token: "forbidden"}).Error
		}))
		require.NoError(t, db.Create(&models.Client{UUID: "after-error", Token: "after-error"}).Error)
	})
}
