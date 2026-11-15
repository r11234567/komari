package migrations

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/komari-monitor/komari/database/metricstore"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/database/trafficledger"
	"github.com/komari-monitor/komari/pkg/metric"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestLegacyMonitoringDirectTelemetryImportTargetsAndResume(t *testing.T) {
	for _, mode := range []string{"fresh", "migrated", "retired", "online"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			cfg := &metricstore.MetricStoreConfig{Driver: "sqlite", DSN: filepath.Join(dir, "metrics.db"), ArchiveDirectory: filepath.Join(dir, "archive"), TablePrefix: "metric_"}
			at := time.Now().UTC().Add(-6 * 24 * time.Hour).Truncate(time.Hour)
			if mode != "fresh" {
				s, err := metric.Open(ctx, metric.SQLite(cfg.DSN, metric.WithRetiredLegacyStorage(), metric.WithSQLiteProfile(metric.SQLiteProfileDurable)))
				if err != nil {
					t.Fatal(err)
				}
				if err = metricstore.EnsureBuiltinMetricDefinitions(ctx, s); err != nil {
					t.Fatal(err)
				}
				tc := metric.TelemetryConfig{ArchiveDirectory: cfg.ArchiveDirectory}
				if mode != "online" {
					if err = s.MigrateLegacy(ctx, tc, nil); err != nil {
						t.Fatal(err)
					}
					if err = s.CompleteLegacyLedgerMigration(ctx, tc); err != nil {
						t.Fatal(err)
					}
				}
				if err = s.EnableTelemetry(ctx, tc); err != nil {
					t.Fatal(err)
				}
				if mode == "online" {
					_, err = s.Telemetry().Accept(ctx, metric.TelemetryBatch{Source: "existing", Identity: "online/1", Points: []metric.Point{{MetricName: metricstore.MetricCPU, EntityID: "existing", Timestamp: time.Now().UTC(), Value: 99}}}, time.Now().UTC())
					if err != nil {
						t.Fatal(err)
					}
				}
				if mode == "retired" {
					if _, err = s.Telemetry().CompactSQLite(ctx, filepath.Join(dir, "replacement.db"), filepath.Join(dir, "backup"), true, nil); err != nil {
						t.Fatal(err)
					}
				}
				s.Close()
				if mode == "retired" {
					cfg.DSN = filepath.Join(dir, "replacement.db")
				}
			}
			source, err := gorm.Open(sqlite.Open(filepath.Join(dir, "source.db")), &gorm.Config{})
			if err != nil {
				t.Fatal(err)
			}
			sourceSQL, _ := source.DB()
			defer sourceSQL.Close()
			if err = source.AutoMigrate(&models.Record{}, &models.PingRecord{}); err != nil {
				t.Fatal(err)
			}
			if err = source.Table("records_long_term").AutoMigrate(&models.Record{}); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 120; i++ {
				if err = source.Create(&models.Record{Client: "node", Time: at.Add(time.Duration(i) * time.Second), Cpu: 10}).Error; err != nil {
					t.Fatal(err)
				}
			}
			// The old upsert contract preferred exact records over overlapping long
			// term rows. Explicitly changing import options must not shift a cursor.
			if err = source.Table("records_long_term").Create(&models.Record{Client: "node", Time: at, Cpu: 999}).Error; err != nil {
				t.Fatal(err)
			}
			if err = source.Table("records_long_term").Create(&models.Record{Client: "node", Time: at.Add(3 * time.Minute), Cpu: 20}).Error; err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				if err = source.Create(&models.PingRecord{Client: "node", TaskId: 7, Time: at.Add(time.Duration(i) * time.Second), Value: 30 - i*31}).Error; err != nil {
					t.Fatal(err)
				}
			}
			for attempt := 0; attempt < 3; attempt++ {
				s, err := metricstore.OpenStoreForMigration(ctx, cfg, 30)
				if err != nil {
					t.Fatal(err)
				}
				if s.Telemetry() == nil {
					t.Fatal("import target did not activate telemetry")
				}
				if _, err = MigrateLegacyMonitoringWithOptions(ctx, source, s, LegacyMonitoringImportOptions{IncludeLongTerm: attempt > 0}, nil); err != nil {
					t.Fatal(err)
				}
				q := metric.AggregateQuery{Query: metric.Query{MetricName: metricstore.MetricCPU, EntityID: "node", Start: at, End: at.Add(30*time.Minute - time.Nanosecond)}, Interval: 30 * time.Minute, Aggregation: metric.AggSum}
				r, err := s.Telemetry().Chart(ctx, q, time.Now().UTC())
				want, count := float64(1200), 120
				if attempt > 0 {
					want, count = 1220, 121
				}
				if err != nil || len(r.Points) != 1 || r.Points[0].Value != want || r.Points[0].Count != count {
					t.Fatal(mode, attempt, r, err)
				}
				s.Close()
			}
			check, err := sql.Open("sqlite3", cfg.DSN)
			if err != nil {
				t.Fatal(err)
			}
			defer check.Close()
			for _, table := range []string{"metric_point_values", "metric_point_blocks", "metric_rollup_values", "metric_rollup_blocks"} {
				var exists, n int
				if err = check.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE name=?", table).Scan(&exists); err != nil {
					t.Fatal(err)
				}
				if exists > 0 {
					if err = check.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil || n != 0 {
						t.Fatal("legacy SQL staging grew", table, n, err)
					}
				}
			}
			var hot int
			if err = check.QueryRow("SELECT COUNT(*) FROM metric_telemetry_hot WHERE observed<?", time.Now().Add(-15*time.Minute).UnixNano()).Scan(&hot); err != nil || hot != 0 {
				t.Fatal("old imported raw remains in SQL", hot, err)
			}
		})
	}
}

func TestLegacyMonitoringDirectLedgerPreservesFirstOnlineBaseline(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := gorm.Open(sqlite.Open(filepath.Join(dir, "komari.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	defer sqlDB.Close()
	modelsToCreate := append(trafficledger.TelemetryModels(), &models.Record{}, &models.Client{}, &models.TrafficDailyLedger{}, &models.TrafficCycleFirstDay{})
	if err = db.AutoMigrate(modelsToCreate...); err != nil {
		t.Fatal(err)
	}
	if err = db.Create(&models.Client{UUID: "node"}).Error; err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Add(-time.Minute)
	for i := 0; i < 2; i++ {
		if err = db.Create(&models.Record{Client: "node", Time: at.Add(time.Duration(i) * time.Second), NetTotalUp: int64(100 + i*30), NetTotalDown: int64(200 + i*50), TrafficUp: int64(i * 30), TrafficDown: int64(i * 50)}).Error; err != nil {
			t.Fatal(err)
		}
	}
	cfg := &metricstore.MetricStoreConfig{Driver: "sqlite", DSN: filepath.Join(dir, "metrics.db"), TablePrefix: "metric_", ArchiveDirectory: filepath.Join(dir, "archive")}
	s, err := metricstore.OpenStoreForMigration(ctx, cfg, 30)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err = MigrateLegacyMonitoring(ctx, db, s, nil); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err = MigrateLegacyMonitoringLedger(ctx, db, s); err != nil {
			t.Fatal(err)
		}
	}
	var daily models.TrafficDailyLedger
	if err = db.Where("client=? AND day=?", "node", trafficledger.BeijingDay(at).Format(time.DateOnly)).First(&daily).Error; err != nil {
		t.Fatal(err)
	}
	if daily.UpBytes != 30 || daily.DownBytes != 50 {
		t.Fatal("traffic ledger changed on direct upgrade/retry", daily)
	}
	var state models.TelemetryTrafficState
	if err = db.Where("client=?", "node").First(&state).Error; err != nil {
		t.Fatal(err)
	}
	if state.TotalUp != 130 || state.TotalDown != 250 {
		t.Fatal("exact final traffic baseline lost", state)
	}
	_, err = s.Telemetry().Accept(ctx, metric.TelemetryBatch{Source: "node", Identity: "first-online", Traffic: &metric.TelemetryTraffic{TotalUp: 137, TotalDown: 261, ObservedAt: at.Add(2 * time.Second).UnixNano()}}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	var event metric.TelemetryTrafficEvent
	_, err = s.Telemetry().DeliverTraffic(ctx, 1, func(_ context.Context, e metric.TelemetryTrafficEvent) error { event = e; return nil })
	if err != nil || event.Traffic.DeltaUp != 7 || event.Traffic.DeltaDown != 11 {
		t.Fatal("first online report recounted historical bytes", event, err)
	}
}
