package metricstore

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/komari-monitor/komari/pkg/metric"
)

func TestTelemetryDrainedSummarySleepsUntilInput(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "metrics.db")
	s, err := metric.Open(ctx, metric.SQLite(path, metric.WithRetiredLegacyStorage(), metric.WithSQLiteProfile(metric.SQLiteProfileDurable)))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.CreateMetric(ctx, metric.Definition{Name: "cpu", RetentionDays: 30}); err != nil {
		t.Fatal(err)
	}
	if err = s.EnableTelemetry(ctx, metric.TelemetryConfig{ArchiveDirectory: filepath.Join(dir, "archive")}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err = ensureTelemetrySchedule(ctx, s.Telemetry(), now.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec("UPDATE metric_telemetry_schedule SET due=? WHERE task='summary'", now.UnixNano()); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Telemetry().Accept(ctx, metric.TelemetryBatch{Source: "node", Identity: "one", Points: []metric.Point{{MetricName: "cpu", EntityID: "node", Timestamp: now, Value: 1}}}, now); err != nil {
		t.Fatal(err)
	}
	result, err := runTelemetryPass(ctx, s, now)
	if err != nil || result.Written != 1 {
		t.Fatal(result, err)
	}
	var summary, tiers int64
	if err = db.QueryRow("SELECT due FROM metric_telemetry_schedule WHERE task='summary'").Scan(&summary); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow("SELECT due FROM metric_telemetry_schedule WHERE task='tiers'").Scan(&tiers); err != nil {
		t.Fatal(err)
	}
	if time.Unix(0, summary).Before(now.Add(59*time.Minute)) || time.Unix(0, tiers).Before(now.Add(time.Hour)) {
		t.Fatal("drained summary caused 200ms retry or immediate tiers wakeup", time.Unix(0, summary), time.Unix(0, tiers))
	}
}

func TestTelemetryWorkerWakesAndStopsWithoutHoldingStore(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := metric.Open(ctx, metric.SQLite(filepath.Join(dir, "metrics.db"), metric.WithRetiredLegacyStorage(), metric.WithSQLiteProfile(metric.SQLiteProfileDurable)))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.CreateMetric(ctx, metric.Definition{Name: "cpu", RetentionDays: 30}); err != nil {
		t.Fatal(err)
	}
	if err = s.EnableTelemetry(ctx, metric.TelemetryConfig{ArchiveDirectory: filepath.Join(dir, "archive")}); err != nil {
		t.Fatal(err)
	}
	restore := SwapStoreForTest(s)
	defer restore()
	priorConsumer := activeTelemetryConsumer.Load()
	ConfigureTelemetryConsumer(nil)
	defer activeTelemetryConsumer.Store(priorConsumer)
	StartTelemetryScheduler()
	defer StopTelemetryScheduler(ctx)
	write := func(id string) {
		at := time.Now().UTC()
		if _, err := s.Telemetry().Accept(ctx, metric.TelemetryBatch{Source: "node", Identity: id, Points: []metric.Point{{MetricName: "cpu", EntityID: "node", Timestamp: at, Value: 3}}}, at); err != nil {
			t.Fatal(err)
		}
	}
	wait := func(want int64) {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			d, err := s.Telemetry().Diagnostics(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if d.Summarized == want {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("worker did not advance summary cursor", want)
	}
	write("first")
	wait(1)
	write("second")
	wait(2)
	stop, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err = StopTelemetryScheduler(stop); err != nil {
		t.Fatal("idle worker held Store lease", err)
	}
	if err = storeOperations.Acquire(stop); err != nil {
		t.Fatal(err)
	}
	storeOperations.Release()
}
