package cmd

import (
	"context"
	"github.com/komari-monitor/komari/cmd/flags"
	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/metricstore"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/pkg/metric"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestImportLegacyHistoryProductionCommand(t *testing.T) {
	if os.Getenv("KOMARI_TEST_HISTORY_EXEC") == "1" {
		RootCmd.SetArgs([]string{"import-legacy-history", "--source", "./source.db", "--confirm-offline"})
		if err := RootCmd.Execute(); err != nil {
			t.Fatal(err)
		}
		return
	}
	mode := os.Getenv("KOMARI_TEST_HISTORY_CLI")
	if mode == "" {
		for _, mode := range []string{"fresh", "retired"} {
			t.Run(mode, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestImportLegacyHistoryProductionCommand$", "-test.v")
				cmd.Dir = t.TempDir()
				cmd.Env = append(os.Environ(), "KOMARI_TEST_HISTORY_CLI="+mode)
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("CLI %s: %v\n%s", mode, err, out)
				}
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
	db := dbcore.GetDBInstance()
	if err := db.Create(&models.Client{UUID: "node", Name: "node"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&models.PingTask{Id: 1, Name: "task"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := a.Shutdown(); err != nil {
		t.Fatal(err)
	}
	cfg := metric.TelemetryConfig{ArchiveDirectory: "./data/telemetry"}
	if mode == "retired" {
		s, err := metric.Open(ctx, metric.SQLite("./data/metrics.db", metric.WithRetiredLegacyStorage(), metric.WithSQLiteProfile(metric.SQLiteProfileDurable)))
		if err != nil {
			t.Fatal(err)
		}
		if err = metricstore.EnsureBuiltinMetricDefinitions(ctx, s); err != nil {
			t.Fatal(err)
		}
		if err = s.MigrateLegacy(ctx, cfg, nil); err != nil {
			t.Fatal(err)
		}
		if err = s.CompleteLegacyLedgerMigration(ctx, cfg); err != nil {
			t.Fatal(err)
		}
		if err = s.EnableTelemetry(ctx, cfg); err != nil {
			t.Fatal(err)
		}
		if _, err = s.Telemetry().CompactSQLite(ctx, "./data/retired.db", "./data/rollback", true, nil); err != nil {
			t.Fatal(err)
		}
		s.Close()
		// Replace only this disposable test's source using the offline output.
		if err = os.Rename("./data/metrics.db", "./data/old.db"); err != nil {
			t.Fatal(err)
		}
		if err = os.Rename("./data/retired.db", "./data/metrics.db"); err != nil {
			t.Fatal(err)
		}
	}
	path, err := filepath.Abs("./source.db")
	if err != nil {
		t.Fatal(err)
	}
	source, err := gorm.Open(sqlite.Open(path), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err = source.AutoMigrate(&models.Client{}, &models.PingTask{}, &models.Record{}, &models.PingRecord{}); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Hour)
	for _, v := range []any{&models.Client{UUID: "node", Name: "node"}, &models.PingTask{Id: 1, Name: "task"}, &models.Record{Client: "node", Time: at, Cpu: 42}, &models.PingRecord{Client: "node", TaskId: 1, Time: at, Value: 17}} {
		if err = source.Create(v).Error; err != nil {
			t.Fatal(err)
		}
	}
	sqlSource, _ := source.DB()
	sqlSource.Close()
	for i := 0; i < 2; i++ {
		command := exec.Command(os.Args[0], "-test.run=^TestImportLegacyHistoryProductionCommand$", "-test.v")
		command.Env = append(os.Environ(), "KOMARI_TEST_HISTORY_EXEC=1")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("CLI attempt %d: %v\n%s", i, err, output)
		}
	}
	s, err := metric.Open(ctx, metric.SQLite("./data/metrics.db", metric.WithRetiredLegacyStorage(), metric.WithSQLiteProfile(metric.SQLiteProfileDurable)))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.EnableTelemetry(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	r, err := s.Telemetry().Chart(ctx, metric.AggregateQuery{Query: metric.Query{MetricName: metricstore.MetricCPU, EntityID: "node", Start: at, End: at.Add(time.Hour - time.Nanosecond)}, Interval: time.Hour, Aggregation: metric.AggSum}, time.Now())
	if err != nil || len(r.Points) != 1 || r.Points[0].Value != 42 || r.Points[0].Count != 1 {
		t.Fatal("command hid or duplicated imported history", r, err)
	}
}
