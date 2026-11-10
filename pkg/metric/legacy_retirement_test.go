package metric

import (
	"context"
	"fmt"
	"math"
	"path/filepath"
	"testing"
	"time"
)

func TestRetiredStorageKeepsOldBlocksReadableWithoutMutations(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "legacy.db")
	s, err := Open(ctx, SQLite(dsn))
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Add(-72 * time.Hour).Truncate(time.Second)
	if err := s.CreateMetric(ctx, Definition{Name: "cpu", Type: TypeGauge, RetentionDays: 1}); err != nil {
		t.Fatal(err)
	}
	for i := range 10 {
		if err := s.Write(ctx, Point{MetricName: "cpu", EntityID: "node", Timestamp: base.Add(time.Duration(i) * time.Second), Value: float64(i)}); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.sealSQLiteV4PointsTx(ctx, tx, "cpu", math.MaxInt64, 0, 0); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	// Any accidental startup migration, maintenance rewrite, or retention
	// deletion fails the test at the database boundary, not via a mocked hook.
	for _, table := range []string{s.tables.pointBlocks, s.tables.rollupBlocks, s.tables.pointAxes, s.tables.rollupAxes} {
		for _, op := range []string{"INSERT", "UPDATE", "DELETE"} {
			q := fmt.Sprintf(`CREATE TRIGGER reject_%s_%s BEFORE %s ON %s BEGIN SELECT RAISE(ABORT, 'retired block mutation'); END`, table, op, op, table)
			if _, err := s.db.ExecContext(ctx, q); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, SQLite(dsn, WithRetiredLegacyStorage(), WithRollupPolicy(RollupPolicy{RawRetention: 2 * time.Minute, Tiers: []RollupTier{{Interval: time.Minute, Retention: 24 * time.Hour}}})))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for range 3 {
		if n, err := s.Compact(ctx, time.Now()); err != nil || n != 0 {
			t.Fatalf("compact: %d %v", n, err)
		}
		if n, err := s.CleanupExpired(ctx, time.Now()); err != nil || n != 0 {
			t.Fatalf("cleanup: %d %v", n, err)
		}
	}
	if err := s.ReclaimSpace(ctx); err != nil {
		t.Fatal(err)
	}
	points, err := s.Query(ctx, Query{MetricName: "cpu", EntityID: "node", Start: base, End: base.Add(time.Minute)})
	if err != nil || len(points) != 10 {
		t.Fatalf("legacy points: %d %v", len(points), err)
	}
	for i, p := range points {
		if p.Value != float64(i) {
			t.Fatalf("point %d: %+v", i, p)
		}
	}
	// New writes stay in ordinary rows and can coexist with the sealed history.
	if err := s.Write(ctx, Point{MetricName: "cpu", EntityID: "node", Timestamp: base.Add(30 * time.Second), Value: 42}); err != nil {
		t.Fatal(err)
	}
	if n, err := s.CompactMetricStep(ctx, "cpu", time.Now()); err != nil || n != 0 {
		t.Fatalf("step: %d %v", n, err)
	}
	points, err = s.Query(ctx, Query{MetricName: "cpu", EntityID: "node", Start: base, End: base.Add(time.Minute)})
	if err != nil || len(points) != 11 {
		t.Fatalf("combined points: %d %v", len(points), err)
	}
}

func TestRetiredStorageFreshAndReopenedNeverCreatesBlocks(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "fresh.db")
	for pass := range 2 {
		s, err := Open(ctx, SQLite(dsn, WithRetiredLegacyStorage()))
		if err != nil {
			t.Fatal(err)
		}
		if pass == 0 {
			if err := s.CreateMetric(ctx, Definition{Name: "cpu", Type: TypeGauge, RetentionDays: 30}); err != nil {
				t.Fatal(err)
			}
			if err := s.Write(ctx, Point{MetricName: "cpu", EntityID: "node", Timestamp: time.Now().Add(-time.Hour), Value: 15}); err != nil {
				t.Fatal(err)
			}
		}
		for _, table := range []string{s.tables.pointBlocks, s.tables.rollupBlocks} {
			var n int
			if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&n); err != nil || n != 0 {
				t.Fatalf("%s: %d %v", table, n, err)
			}
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRetiredStorageInspectionDoesNotScanSource(t *testing.T) {
	cfg := SQLite(filepath.Join(t.TempDir(), "fresh.db"), WithRetiredLegacyStorage())
	s, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	summary, err := InspectSQLiteMigration(context.Background(), cfg)
	if err != nil || summary.Required || summary.SourceRows != 0 || summary.Layout != "legacy-retired" {
		t.Fatalf("inspection: %+v %v", summary, err)
	}
}

func TestRetiredStorageDoesNotUpgradeV3DataOrVersion(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "v3.db")
	s := createSQLiteV3OnlyStore(t, ctx, dsn)
	if err := s.CreateMetric(ctx, Definition{Name: "cpu", Type: TypeGauge, RetentionDays: 30}); err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Add(-time.Hour)
	if err := s.Write(ctx, Point{MetricName: "cpu", EntityID: "node", Timestamp: base, Value: 3}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for _, migrate := range []bool{true, false} {
		s, err := Open(ctx, SQLite(dsn, WithRetiredLegacyStorage(), WithAutoMigrate(migrate)))
		if err != nil {
			t.Fatal(err)
		}
		var version, blocks int
		if err := s.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
			t.Fatal(err)
		}
		if version >= sqliteStorageVersionPingMerge || s.sqlitePingMerged {
			t.Fatalf("unexpected data version upgrade: %d", version)
		}
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+s.tables.pointBlocks).Scan(&blocks); err != nil || blocks != 0 {
			t.Fatalf("blocks: %d %v", blocks, err)
		}
		got, err := s.Query(ctx, Query{MetricName: "cpu", EntityID: "node", Start: base.Add(-time.Second), End: base.Add(time.Second)})
		if err != nil || len(got) != 1 || got[0].Value != 3 {
			t.Fatalf("V3 history: %+v %v", got, err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
