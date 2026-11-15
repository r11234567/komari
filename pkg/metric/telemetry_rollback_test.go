package metric

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestTelemetryOfflineRollbackHotColdAndErasure(t *testing.T) {
	s, e := telemetryFixture(t, "")
	ctx := context.Background()
	if err := s.CreateMetric(ctx, Definition{Name: "cpu", RetentionDays: 30}); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Hour)
	if _, err := e.Accept(ctx, telemetryBatch("old", at, 99), at); err != nil {
		t.Fatal(err)
	}
	if err := s.EnableTelemetry(ctx, e.cfg); err != nil {
		t.Fatal(err)
	}
	e = s.Telemetry()
	if _, err := e.EraseSeries(ctx, Query{MetricName: "cpu", EntityID: "node"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Accept(ctx, telemetryBatch("cold", at.Add(time.Minute), 2, 3), at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	summarizeAll(t, e)
	if _, err := e.SealDue(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	for {
		ok, err := e.ArchiveNext(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			break
		}
	}
	for {
		n, err := e.DeleteArchivedChunk(ctx, time.Now(), 512)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
	}
	if _, err := e.Accept(ctx, telemetryBatch("hot", time.Now().UTC(), 7), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "rollback.db")
	if err := e.RollbackLegacy(ctx, output, nil); err != nil {
		t.Fatal(err)
	}
	old, err := Open(ctx, SQLite(output, WithRetiredLegacyStorage()))
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	points, err := old.Query(ctx, Query{MetricName: "cpu", EntityID: "node", Start: at, End: time.Now().Add(time.Minute)})
	if err != nil || len(points) != 3 || points[0].Value != 2 || points[1].Value != 3 || points[2].Value != 7 {
		t.Fatal(points, err)
	}
	if err = old.RequireTelemetryMigration(ctx); err == nil {
		t.Fatal("rollback reused stale new-engine state")
	}
	if err = e.RollbackLegacy(ctx, output, nil); err == nil {
		t.Fatal("existing output overwritten")
	}
	for {
		ok, err := e.ExpireArchiveNext(ctx, time.Now().Add(40*24*time.Hour), 30*24*time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			break
		}
	}
	if err = e.RollbackLegacy(ctx, filepath.Join(t.TempDir(), "incomplete.db"), nil); !errors.Is(err, ErrTelemetryCoverage) {
		t.Fatal("partial rollback accepted", err)
	}
}
