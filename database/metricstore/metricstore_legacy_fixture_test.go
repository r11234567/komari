//go:build metric_legacy_fixture

package metricstore

import (
	"context"
	"database/sql"
	"fmt"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/pkg/metric"
	v1 "github.com/komari-monitor/komari/protocol/v1"
	"path/filepath"
	"testing"
	"time"
)

func TestCompactCleansExpiredRawPointsWhenDownsamplingDisabled(t *testing.T) {
	ctx := context.Background()
	s, err := metric.Open(ctx, metric.SQLite(":memory:", metric.WithMaxOpenConns(1)))
	if err != nil {
		t.Fatalf("open metric store: %v", err)
	}
	if err := s.UpsertMetric(ctx, metric.Definition{
		Name:          "raw.metric",
		Type:          metric.TypeGauge,
		RetentionDays: 1,
	}); err != nil {
		t.Fatalf("upsert metric: %v", err)
	}

	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	if err := s.WriteBatch(ctx, []metric.Point{
		{MetricName: "raw.metric", EntityID: "node", Timestamp: now.Add(-48 * time.Hour), Value: 1},
		{MetricName: "raw.metric", EntityID: "node", Timestamp: now.Add(-time.Hour), Value: 2},
	}); err != nil {
		t.Fatalf("write points: %v", err)
	}

	storeMu.Lock()
	oldStore := store
	oldCompactAt := compactAt
	store = s
	compactAt = 0
	storeMu.Unlock()
	defer func() {
		storeMu.Lock()
		store = oldStore
		compactAt = oldCompactAt
		storeMu.Unlock()
		_ = s.Close()
	}()

	if _, err := Compact(ctx, now); err != nil {
		t.Fatalf("compact: %v", err)
	}
	points, err := s.Query(ctx, metric.Query{
		MetricName: "raw.metric",
		EntityID:   "node",
		Start:      now.Add(-72 * time.Hour),
		End:        now,
	})
	if err != nil {
		t.Fatalf("query points: %v", err)
	}
	if len(points) != 1 || points[0].Value != 2 {
		t.Fatalf("expected only the retained raw point, got %#v", points)
	}
}

func TestCompactContinuesAfterOneMetricFails(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "compact.db")
	s, err := metric.Open(ctx, metric.SQLite(dsn,
		metric.WithMaxOpenConns(1),
		metric.WithRollupPolicy(defaultRollupPolicy()),
	))
	if err != nil {
		t.Fatalf("open metric store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	for _, name := range []string{"a.invalid", "b.healthy"} {
		if err := s.CreateMetric(ctx, metric.Definition{Name: name, Type: metric.TypeGauge, RetentionDays: 1}); err != nil {
			t.Fatalf("create metric %s: %v", name, err)
		}
	}

	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	old := now.Add(-time.Hour)
	if err := s.Write(ctx, metric.Point{MetricName: "b.healthy", EntityID: "node", Timestamp: old, Value: 2}); err != nil {
		t.Fatalf("write healthy point: %v", err)
	}

	rawDB, err := sql.Open("sqlite3", dsn)
	if err != nil {
		t.Fatalf("open raw sqlite connection: %v", err)
	}
	_, err = rawDB.ExecContext(ctx, `INSERT INTO metric_points
		(metric_name, entity_id, tags_hash, ts_nano, value, tags, labels, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		"a.invalid", "node", "invalid", old.UnixNano(), 1, "not-json", "{}", now.UnixNano(),
	)
	_ = rawDB.Close()
	if err != nil {
		t.Fatalf("insert malformed point: %v", err)
	}

	storeMu.Lock()
	previousStore := store
	previousCompactAt := compactAt
	store = s
	compactAt = 0
	storeMu.Unlock()
	t.Cleanup(func() {
		storeMu.Lock()
		store = previousStore
		compactAt = previousCompactAt
		storeMu.Unlock()
	})

	if _, err := Compact(ctx, now); err == nil {
		t.Fatal("expected malformed metric to fail compaction")
	}
	points, err := s.Query(ctx, metric.Query{
		MetricName: "b.healthy",
		EntityID:   "node",
		Start:      old.Add(-time.Minute),
		End:        now,
	})
	if err != nil {
		t.Fatalf("query healthy raw points: %v", err)
	}
	if len(points) != 0 {
		t.Fatalf("healthy metric was blocked by another metric failure: %s", fmt.Sprint(points))
	}
}

func TestCompactStepProcessesOnlyOneMetric(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "compact-step.db")
	s, err := metric.Open(ctx, metric.SQLite(dsn,
		metric.WithMaxOpenConns(1),
		metric.WithRollupPolicy(defaultRollupPolicy()),
	))
	if err != nil {
		t.Fatalf("open metric store: %v", err)
	}
	for _, name := range []string{"a.metric", "b.metric", "c.metric"} {
		if err := s.UpsertMetric(ctx, metric.Definition{Name: name, Type: metric.TypeGauge, RetentionDays: 1}); err != nil {
			t.Fatalf("upsert metric %s: %v", name, err)
		}
	}

	now := time.Date(2026, 7, 25, 0, 0, 0, 0, time.UTC)
	for i, name := range []string{"a.metric", "b.metric", "c.metric"} {
		if err := s.Write(ctx, metric.Point{MetricName: name, EntityID: "node", Timestamp: now.Add(-time.Hour), Value: float64(i + 1)}); err != nil {
			t.Fatalf("write metric %s: %v", name, err)
		}
	}
	installCompactStepTestStore(t, s, 0)

	written, completed, err := CompactStep(ctx, now)
	if err != nil {
		t.Fatalf("first compact step: %v", err)
	}
	if written == 0 || completed {
		t.Fatalf("first compact step = written %d, completed %v; want a partial cycle with rollups", written, completed)
	}
	status := GetRuntimeStatus()
	if status.Compacting || status.CurrentMetric != "a.metric" || status.Progress != 1 || status.Total != 3 {
		t.Fatalf("runtime status after first step = %#v", status)
	}
	if status.CycleStartedAt.IsZero() || status.LastStepAt.IsZero() || status.NextCheckpointAt.IsZero() {
		t.Fatalf("runtime status timestamps were not recorded: %#v", status)
	}
	if status.CycleWritten != written {
		t.Fatalf("runtime cycle written = %d, want %d", status.CycleWritten, written)
	}
	assertRawMetricCount(t, dsn, "a.metric", 0)
	assertRawMetricCount(t, dsn, "b.metric", 1)
	assertRawMetricCount(t, dsn, "c.metric", 1)

	if _, completed, err = CompactStep(ctx, now); err != nil || completed {
		t.Fatalf("second compact step = completed %v, err %v; want partial cycle", completed, err)
	}
	assertRawMetricCount(t, dsn, "b.metric", 0)
	assertRawMetricCount(t, dsn, "c.metric", 1)

	if _, completed, err = CompactStep(ctx, now); err != nil || !completed {
		t.Fatalf("third compact step = completed %v, err %v; want completed cycle", completed, err)
	}
	status = GetRuntimeStatus()
	if status.Progress != 3 || status.Total != 3 || status.LastCycleCompletedAt.IsZero() {
		t.Fatalf("runtime status after completed cycle = %#v", status)
	}
	if status.CheckpointPending || status.LastCheckpointSuccessAt.IsZero() || status.LastError != "" {
		t.Fatalf("runtime checkpoint status after completed cycle = %#v", status)
	}
	assertRawMetricCount(t, dsn, "c.metric", 0)
}

func TestCompactStepDefersCleanupAndCheckpointUntilCycleEnd(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "compact-step-maintenance.db")
	s, err := metric.Open(ctx, metric.SQLite(dsn, metric.WithMaxOpenConns(1)))
	if err != nil {
		t.Fatalf("open metric store: %v", err)
	}
	for _, name := range []string{"a.metric", "b.metric"} {
		if err := s.UpsertMetric(ctx, metric.Definition{Name: name, Type: metric.TypeGauge, RetentionDays: 1}); err != nil {
			t.Fatalf("upsert metric %s: %v", name, err)
		}
	}
	now := time.Date(2026, 7, 25, 0, 0, 0, 0, time.UTC)
	for _, name := range []string{"a.metric", "b.metric"} {
		if err := s.Write(ctx, metric.Point{MetricName: name, EntityID: "node", Timestamp: now.Add(-48 * time.Hour), Value: 1}); err != nil {
			t.Fatalf("write metric %s: %v", name, err)
		}
	}
	installCompactStepTestStore(t, s, 0)

	written, completed, err := CompactStep(ctx, now)
	if err != nil || written != 0 || completed {
		t.Fatalf("first compact step = written %d, completed %v, err %v", written, completed, err)
	}
	assertRawMetricCount(t, dsn, "a.metric", 1)
	assertRawMetricCount(t, dsn, "b.metric", 1)
	if size := fileSize(t, dsn+"-wal"); size == 0 {
		t.Fatal("SQLite WAL was truncated before the compact cycle completed")
	}

	written, completed, err = CompactStep(ctx, now)
	if err != nil || written != 0 || !completed {
		t.Fatalf("second compact step = written %d, completed %v, err %v", written, completed, err)
	}
	assertRawMetricCount(t, dsn, "a.metric", 0)
	assertRawMetricCount(t, dsn, "b.metric", 0)
	if size := fileSize(t, dsn+"-wal"); size != 0 {
		t.Fatalf("SQLite WAL size after completed compact cycle = %d, want 0", size)
	}
}

func TestFinishCompactCycleCleansExpiredDataEveryCycle(t *testing.T) {
	ctx := context.Background()
	s, err := metric.Open(ctx, metric.SQLite(":memory:", metric.WithMaxOpenConns(1)))
	if err != nil {
		t.Fatalf("open metric store: %v", err)
	}
	defer s.Close()
	if err := s.UpsertMetric(ctx, metric.Definition{Name: "retention.metric", Type: metric.TypeGauge, RetentionDays: 1}); err != nil {
		t.Fatalf("upsert metric: %v", err)
	}
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	for cycle := 1; cycle <= 2; cycle++ {
		old := now.Add(-time.Duration(48+cycle) * time.Hour)
		if err := s.Write(ctx, metric.Point{MetricName: "retention.metric", EntityID: "node", Timestamp: old, Value: float64(cycle)}); err != nil {
			t.Fatalf("cycle %d write old point: %v", cycle, err)
		}
		if err := finishCompactCycle(ctx, s, now, false); err != nil {
			t.Fatalf("cycle %d cleanup: %v", cycle, err)
		}
		points, err := s.Query(ctx, metric.Query{MetricName: "retention.metric", EntityID: "node", Start: old.Add(-time.Second), End: now})
		if err != nil || len(points) != 0 {
			t.Fatalf("cycle %d kept expired data: points=%d err=%v", cycle, len(points), err)
		}
	}
}

func TestCompactStepAdvancesAfterMetricFailure(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "compact-step-failure.db")
	s, err := metric.Open(ctx, metric.SQLite(dsn,
		metric.WithMaxOpenConns(1),
		metric.WithRollupPolicy(defaultRollupPolicy()),
	))
	if err != nil {
		t.Fatalf("open metric store: %v", err)
	}
	for _, name := range []string{"a.invalid", "b.healthy"} {
		if err := s.CreateMetric(ctx, metric.Definition{Name: name, Type: metric.TypeGauge, RetentionDays: 1}); err != nil {
			t.Fatalf("create metric %s: %v", name, err)
		}
	}
	now := time.Date(2026, 7, 25, 0, 0, 0, 0, time.UTC)
	old := now.Add(-48 * time.Hour)
	if err := s.Write(ctx, metric.Point{MetricName: "b.healthy", EntityID: "node", Timestamp: old, Value: 2}); err != nil {
		t.Fatalf("write healthy point: %v", err)
	}
	rawDB, err := sql.Open("sqlite3", dsn)
	if err != nil {
		t.Fatalf("open raw sqlite connection: %v", err)
	}
	_, err = rawDB.ExecContext(ctx, `INSERT INTO metric_points
		(metric_name, entity_id, tags_hash, ts_nano, value, tags, labels, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		"a.invalid", "node", "invalid", old.UnixNano(), 1, "not-json", "{}", now.UnixNano(),
	)
	_ = rawDB.Close()
	if err != nil {
		t.Fatalf("insert malformed point: %v", err)
	}
	installCompactStepTestStore(t, s, 0)

	if _, completed, err := CompactStep(ctx, now); err == nil || completed {
		t.Fatalf("failed metric step = completed %v, err %v; want failure in partial cycle", completed, err)
	}
	if compactAt != 1 {
		t.Fatalf("cursor after failed metric = %d, want 1", compactAt)
	}
	if _, completed, err := CompactStep(ctx, now); err == nil || !completed {
		t.Fatalf("healthy metric step = completed %v, err %v; want completed cycle retaining cleanup error", completed, err)
	}
	assertRawMetricCount(t, dsn, "b.healthy", 0)
}

func TestGetRecordsByClientAndTimeReadsRollupsAfterRawCompaction(t *testing.T) {
	ctx := context.Background()
	s, err := metric.Open(ctx, metric.SQLite(":memory:",
		metric.WithMaxOpenConns(1),
		metric.WithRollupPolicy(defaultRollupPolicy()),
	))
	if err != nil {
		t.Fatalf("open metric store: %v", err)
	}
	if err := createMetricDefinitions(ctx, s); err != nil {
		t.Fatalf("create metric definitions: %v", err)
	}

	storeMu.Lock()
	oldStore := store
	oldCompactAt := compactAt
	store = s
	compactAt = 0
	storeMu.Unlock()
	defer func() {
		storeMu.Lock()
		store = oldStore
		compactAt = oldCompactAt
		storeMu.Unlock()
		_ = s.Close()
	}()

	now := time.Now().UTC().Truncate(time.Minute)
	ts := now.Add(-time.Hour)
	rec := models.Record{
		Client:         "node-a",
		Time:           ts,
		Cpu:            42.5,
		Ram:            123456,
		RamTotal:       999999,
		Disk:           456789,
		DiskTotal:      777777,
		Load:           0.75,
		Connections:    321,
		ConnectionsUdp: 12,
	}
	if _, err := WriteReport(ctx, v1.Report{
		UUID:      rec.Client,
		UpdatedAt: ts,
		CPU:       v1.CPUReport{Usage: float64(rec.Cpu)},
		Ram:       v1.RamReport{Used: rec.Ram, Total: rec.RamTotal},
		Load:      v1.LoadReport{Load1: float64(rec.Load)},
		Disk:      v1.DiskReport{Used: rec.Disk, Total: rec.DiskTotal},
		Process:   rec.Process,
		Connections: v1.ConnectionsReport{
			TCP: rec.Connections,
			UDP: rec.ConnectionsUdp,
		},
	}); err != nil {
		t.Fatalf("write record: %v", err)
	}
	if _, err := s.Compact(ctx, now); err != nil {
		t.Fatalf("compact raw into rollup: %v", err)
	}
	raw, err := s.Query(ctx, metric.Query{
		MetricName: MetricCPU,
		EntityID:   rec.Client,
		Start:      ts.Add(-time.Minute),
		End:        now,
	})
	if err != nil {
		t.Fatalf("query raw cpu: %v", err)
	}
	if len(raw) != 0 {
		t.Fatalf("expected old raw cpu point to be deleted after compaction, got %d", len(raw))
	}

	got, err := GetRecordsByClientAndTime(ctx, rec.Client, ts.Add(-time.Minute), now)
	if err != nil {
		t.Fatalf("get records: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 reconstructed record from rollup, got %d: %#v", len(got), got)
	}
	if got[0].Cpu == 0 || got[0].Ram == 0 || got[0].Disk == 0 || got[0].Connections == 0 {
		t.Fatalf("record was not reconstructed from rollup: %#v", got[0])
	}

	all, err := GetRecordsByTime(ctx, ts.Add(-time.Minute), now)
	if err != nil {
		t.Fatalf("get all records: %v", err)
	}
	if len(all) != 1 || all[0].Client != rec.Client || all[0].Cpu == 0 {
		t.Fatalf("all-client records were not reconstructed from rollup: %#v", all)
	}
}
