package metricstore

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/komari-monitor/komari/pkg/metric"
)

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
