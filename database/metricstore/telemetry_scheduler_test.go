package metricstore

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/komari-monitor/komari/pkg/metric"
	"github.com/stretchr/testify/require"
)

func TestTelemetryTrafficSurvivesSQLiteWriterContention(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	dir := t.TempDir()
	s, err := metric.Open(ctx, metric.SQLite(filepath.Join(dir, "metrics.db"), metric.WithRetiredLegacyStorage(), metric.WithSQLiteProfile(metric.SQLiteProfileDurable)))
	require.NoError(t, err)
	defer s.Close()
	require.NoError(t, s.CreateMetric(ctx, metric.Definition{Name: "cpu", RetentionDays: 30}))
	require.NoError(t, s.EnableTelemetry(ctx, metric.TelemetryConfig{ArchiveDirectory: filepath.Join(dir, "archive")}))

	ledger, err := sql.Open("sqlite3", filepath.Join(dir, "metadata.db")+"?_busy_timeout=5000&_txlock=immediate&_journal_mode=WAL")
	require.NoError(t, err)
	defer ledger.Close()
	_, err = ledger.ExecContext(ctx, "CREATE TABLE traffic (ingest_id INTEGER PRIMARY KEY)")
	require.NoError(t, err)
	writer, err := ledger.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer writer.Rollback()
	priorConsumer := activeTelemetryConsumer.Load()
	defer activeTelemetryConsumer.Store(priorConsumer)
	ConfigureTelemetryConsumer(func(ctx context.Context, event metric.TelemetryTrafficEvent) error {
		// Keep the other SQLite writer open beyond the former delivery budget.
		released := make(chan error, 1)
		time.AfterFunc(3*time.Second, func() { released <- writer.Rollback() })
		tx, err := ledger.BeginTx(ctx, nil)
		require.NoError(t, <-released)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if _, err = tx.ExecContext(ctx, "INSERT INTO traffic VALUES (?)", event.IngestID); err != nil {
			return err
		}
		return tx.Commit()
	})
	now := time.Now().UTC()
	require.NoError(t, s.Telemetry().EnsureSchedule(ctx, []metric.ScheduleTask{{Name: "traffic", Due: now}}))
	require.NoError(t, ensureTelemetrySchedule(ctx, s.Telemetry(), now.Add(time.Hour)))
	_, err = s.Telemetry().Accept(ctx, metric.TelemetryBatch{
		Source: "node", Identity: "one", Traffic: &metric.TelemetryTraffic{},
		Points: []metric.Point{{MetricName: "cpu", EntityID: "node", Timestamp: now, Value: 1}},
	}, now)
	require.NoError(t, err)
	result, err := runTelemetryPass(ctx, s, now)
	require.NoError(t, err)
	require.Equal(t, 1, result.Cleanups)
	pending, err := s.Telemetry().TrafficPendingFor(ctx, "node")
	require.NoError(t, err)
	require.False(t, pending)
	var count int
	require.NoError(t, ledger.QueryRowContext(ctx, "SELECT COUNT(*) FROM traffic").Scan(&count))
	require.Equal(t, 1, count)
}

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
