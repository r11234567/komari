package metric

import (
	"context"
	"testing"
	"time"
)

func TestTelemetrySparseMigrationDoesNotCommitEmptyWindows(t *testing.T) {
	ctx := context.Background()
	s, e := telemetryFixture(t, "")
	now := time.Date(2025, 4, 15, 12, 0, 0, 0, time.UTC)
	if err := s.CreateMetric(ctx, Definition{Name: "cpu", RetentionDays: 400}); err != nil {
		t.Fatal(err)
	}
	for _, at := range []time.Time{now.Add(-399 * 24 * time.Hour), now.Add(-time.Hour)} {
		if err := s.Write(ctx, Point{MetricName: "cpu", EntityID: "node", Timestamp: at, Value: 7}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec("INSERT INTO "+e.table("progress")+" (task,value) VALUES ('legacy_migration_now',?)", now.UnixNano()); err != nil {
		t.Fatal(err)
	}
	cfg := e.cfg
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.MigrateLegacy(ctx, cfg, nil); err != nil {
		t.Fatal(err)
	}
	var markers int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM " + e.table("progress") + " WHERE task LIKE 'm/%'").Scan(&markers); err != nil || markers != 2 {
		t.Fatal("empty history caused durable writes", markers, err)
	}
	if err := s.MigrateLegacy(ctx, cfg, nil); err != nil {
		t.Fatal(err)
	}
	var repeated int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM " + e.table("progress") + " WHERE task LIKE 'm/%'").Scan(&repeated); err != nil || repeated != markers {
		t.Fatal("reopen changed checkpoints", repeated, err)
	}
}
