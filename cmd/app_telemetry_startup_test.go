package cmd

import (
	"context"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/komari-monitor/komari/cmd/flags"
	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/metricstore"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/pkg/metric"
)

// Exercise the production App lifecycle in separate processes: config/store
// singletons and background workers must not leak between startup scenarios.
func TestAppInitStoresTelemetryLifecycle(t *testing.T) {
	mode := os.Getenv("KOMARI_TEST_STORE_STARTUP")
	if mode == "" {
		for _, mode := range []string{"fresh", "unmigrated", "migrated", "retired"} {
			t.Run(mode, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestAppInitStoresTelemetryLifecycle$", "-test.v")
				cmd.Dir = t.TempDir()
				cmd.Env = append(os.Environ(), "KOMARI_TEST_STORE_STARTUP="+mode)
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("%s startup: %v\n%s", mode, err, out)
				}
				t.Logf("%s", out)
			})
		}
		return
	}
	ctx := context.Background()
	flags.DatabaseFile = "./data/komari.db"
	a := NewApp()
	if err := a.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Shutdown() })
	db := dbcore.GetDBInstance()
	if err := db.Create(&models.Client{UUID: "kept-node", Name: "kept"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&models.PingTask{Id: 1, Name: "kept-task"}).Error; err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour)
	if mode != "fresh" {
		s, err := metric.Open(ctx, metric.SQLite("./data/metrics.db", metric.WithTablePrefix("metric_"), metric.WithRetiredLegacyStorage(), metric.WithSQLiteProfile(metric.SQLiteProfileDurable)))
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		if err = metricstore.EnsureBuiltinMetricDefinitions(ctx, s); err != nil {
			t.Fatal(err)
		}
		points := []metric.Point{
			{MetricName: metricstore.MetricCPU, EntityID: "kept-node", Timestamp: at, Value: 12},
			{MetricName: metricstore.MetricCPU, EntityID: "deleted-node", Timestamp: at, Value: 90},
			{MetricName: metricstore.MetricPingLatency, EntityID: "kept-node", Timestamp: at, Value: 17, Tags: map[string]string{"task_id": "1"}},
			{MetricName: metricstore.MetricPingLatency, EntityID: "kept-node", Timestamp: at, Value: 99, Tags: map[string]string{"task_id": "999"}},
		}
		if err = s.WriteBatch(ctx, points); err != nil {
			t.Fatal(err)
		}
		if mode != "unmigrated" {
			cfg := metric.TelemetryConfig{ArchiveDirectory: "./data/telemetry"}
			if err = s.MigrateLegacy(ctx, cfg, nil); err != nil {
				t.Fatal(err)
			}
			// This fixture has no traffic counters to import. Record the actual
			// completed empty business-ledger phase through its public method.
			if err = s.CompleteLegacyLedgerMigration(ctx, cfg); err != nil {
				t.Fatal(err)
			}
			if mode == "retired" {
				if err = s.EnableTelemetry(ctx, cfg); err != nil {
					t.Fatal(err)
				}
				if _, err = s.Telemetry().CompactSQLite(ctx, "replacement.db", "rollback", true, nil); err != nil {
					t.Fatal(err)
				}
			}
		}
		if err = s.Close(); err != nil {
			t.Fatal(err)
		}
		if mode == "retired" {
			if err = os.Rename("./data/metrics.db", "./data/metrics.db.original"); err != nil {
				t.Fatal(err)
			}
			if err = os.Rename("replacement.db", "./data/metrics.db"); err != nil {
				t.Fatal(err)
			}
		}
	}
	err := a.InitStores()
	if mode == "unmigrated" {
		if err == nil || !strings.Contains(err.Error(), "legacy telemetry exists") {
			t.Fatalf("unmigrated history must reject activation, got %v", err)
		}
		if metricstore.GetStore().Telemetry() != nil {
			t.Fatal("failed cutover enabled telemetry")
		}
		for _, c := range a.cleanups {
			if c.name == "metric-report-batcher" || c.name == "telemetry-scheduler" || c.name == "export-policy" {
				t.Fatalf("writer %s started before migration completed", c.name)
			}
		}
		return
	}
	if err != nil {
		t.Fatalf("App.InitStores: %v", err)
	}
	s := metricstore.GetStore()
	if s.Telemetry() == nil {
		t.Fatal("store did not activate telemetry")
	}
	if mode == "fresh" {
		return
	}
	ids, err := s.AllEntityIDs(ctx)
	if err != nil || !reflect.DeepEqual(ids, []string{"kept-node"}) {
		t.Fatalf("orphan entity cleanup: %v, %v", ids, err)
	}
	tags, err := s.MetricTagValues(ctx, metricstore.MetricPingLatency, "task_id")
	if err != nil || !reflect.DeepEqual(tags, []string{"1"}) {
		t.Fatalf("orphan ping task cleanup: %v, %v", tags, err)
	}
	for _, q := range []struct {
		metric, entity, task string
		want                 float64
		count                int
	}{
		{metricstore.MetricCPU, "kept-node", "", 12, 1},
		{metricstore.MetricCPU, "deleted-node", "", 0, 0},
		{metricstore.MetricPingLatency, "kept-node", "1", 17, 1},
		{metricstore.MetricPingLatency, "kept-node", "999", 0, 0},
	} {
		filter := metric.Query{MetricName: q.metric, EntityID: q.entity, Start: at, End: at.Add(time.Minute - time.Nanosecond)}
		if q.task != "" {
			filter.Tags = map[string]string{"task_id": q.task}
		}
		r, err := s.Telemetry().Chart(ctx, metric.AggregateQuery{Query: filter, Interval: time.Minute, Aggregation: metric.AggAvg}, time.Now())
		if err != nil || len(r.Points) != q.count {
			t.Fatalf("chart %s/%s/%s: %+v, %v", q.metric, q.entity, q.task, r, err)
		}
		if q.count > 0 && (r.Points[0].Value != q.want || r.Points[0].Count != 1) {
			t.Fatalf("valid history changed: %+v", r)
		}
	}
	if mode == "retired" {
		check, err := sql.Open("sqlite3", "file:"+filepath.ToSlash("./data/metrics.db")+"?mode=ro")
		if err != nil {
			t.Fatal(err)
		}
		defer check.Close()
		var legacy int
		if err = check.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE name IN ('metric_points','metric_rollups','metric_point_blocks','metric_rollup_blocks')").Scan(&legacy); err != nil || legacy != 0 {
			t.Fatalf("startup recreated retired tables: %d, %v", legacy, err)
		}
	}
}
