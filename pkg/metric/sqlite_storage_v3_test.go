package metric

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSQLiteStorageV3UsedForNewDatabase(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, SQLiteInDir(t.TempDir()))
	if err != nil {
		t.Fatalf("open new SQLite database: %v", err)
	}
	defer store.Close()
	assertSQLiteV3Schema(t, ctx, store.db)
	if mode := sqlitePragmaInt(t, ctx, store.db, "auto_vacuum"); mode != 2 {
		t.Fatalf("new SQLite auto_vacuum mode = %d, want incremental(2)", mode)
	}
}

func TestSQLiteStorageV3SupportsCustomTablePrefix(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, SQLite(
		sqliteFileDSN(filepath.Join(t.TempDir(), "metrics.db")),
		WithTablePrefix("custom_"),
	))
	if err != nil {
		t.Fatalf("open custom-prefix SQLite database: %v", err)
	}
	defer store.Close()
	for name, wantKind := range map[string]string{
		"custom_points": "view", "custom_rollups": "view", "custom_series": "table",
		"custom_point_values": "table", "custom_rollup_values": "table",
	} {
		kind, err := sqliteObjectType(ctx, store.db, name)
		if err != nil || kind != wantKind {
			t.Fatalf("custom-prefix object %s: kind=%q want=%q err=%v", name, kind, wantKind, err)
		}
	}
}

func TestSQLiteStorageV3DetectedWithoutAutoMigration(t *testing.T) {
	ctx := context.Background()
	dsn := sqliteFileDSN(filepath.Join(t.TempDir(), "metrics.db"))
	store, err := Open(ctx, SQLite(dsn))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateMetric(ctx, Definition{Name: "existing", Type: TypeGauge, RetentionDays: 1}); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()

	store, err = Open(ctx, SQLite(dsn, WithAutoMigrate(false)))
	if err != nil {
		t.Fatalf("open V3 database without auto migration: %v", err)
	}
	defer store.Close()
	if !store.sqliteStorageV3 {
		t.Fatal("V3 storage was not detected without auto migration")
	}
	point := Point{MetricName: "existing", EntityID: "node-a", Timestamp: time.Now().UTC(), Value: 1}
	if err := store.Write(ctx, point); err != nil {
		t.Fatalf("write V3 database without auto migration: %v", err)
	}
}

func TestSQLiteStorageV3IncrementalVacuumReclaimsDeletedPages(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, SQLiteInDir(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.CreateMetric(ctx, Definition{Name: "vacuum", Type: TypeGauge, RetentionDays: 1}); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	points := make([]Point, 5000)
	for i := range points {
		points[i] = Point{MetricName: "vacuum", EntityID: "node-a", Timestamp: base.Add(time.Duration(i) * time.Second), Value: float64(i)}
	}
	if err := store.WriteBatch(ctx, points); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteMetricData(ctx, "vacuum"); err != nil {
		t.Fatal(err)
	}
	beforePages := sqlitePragmaInt(t, ctx, store.db, "page_count")
	beforeFree := sqlitePragmaInt(t, ctx, store.db, "freelist_count")
	if beforeFree == 0 {
		t.Fatal("expected deleted V3 data to leave reclaimable pages")
	}
	if err := store.incrementalSQLiteVacuum(ctx, beforeFree); err != nil {
		t.Fatal(err)
	}
	afterPages := sqlitePragmaInt(t, ctx, store.db, "page_count")
	afterFree := sqlitePragmaInt(t, ctx, store.db, "freelist_count")
	if afterPages > beforePages || afterFree != 0 {
		t.Fatalf("incremental vacuum did not reclaim pages: pages %d->%d free %d->%d", beforePages, afterPages, beforeFree, afterFree)
	}
}

func createLegacySQLiteMetricSchema(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	statements := []string{
		`CREATE TABLE metric_definitions (
		 name VARCHAR(191) PRIMARY KEY, type VARCHAR(32) NOT NULL, unit VARCHAR(64) NOT NULL DEFAULT '',
		 description TEXT NOT NULL DEFAULT '', retention_days INTEGER NOT NULL DEFAULT 0, metadata TEXT NOT NULL,
		 created_at BIGINT NOT NULL, updated_at BIGINT NOT NULL)`,
		`CREATE TABLE metric_points (
		 id INTEGER PRIMARY KEY AUTOINCREMENT, metric_name VARCHAR(191) NOT NULL, entity_id VARCHAR(191) NOT NULL,
		 tags_hash VARCHAR(64) NOT NULL, ts_nano BIGINT NOT NULL, value DOUBLE PRECISION NOT NULL,
		 tags TEXT NOT NULL, labels TEXT NOT NULL, created_at BIGINT NOT NULL,
		 UNIQUE(metric_name, entity_id, tags_hash, ts_nano))`,
		`CREATE INDEX metric__points_metric_entity_time_idx ON metric_points (metric_name, entity_id, ts_nano)`,
		`CREATE INDEX metric__points_metric_time_idx ON metric_points (metric_name, ts_nano)`,
		`CREATE INDEX metric__points_entity_time_idx ON metric_points (entity_id, ts_nano)`,
		`CREATE TABLE metric_rollups (
		 id INTEGER PRIMARY KEY AUTOINCREMENT, metric_name VARCHAR(191) NOT NULL, entity_id VARCHAR(191) NOT NULL,
		 tags_hash VARCHAR(64) NOT NULL, tags TEXT NOT NULL, resolution_nano BIGINT NOT NULL, bucket_nano BIGINT NOT NULL,
		 count BIGINT NOT NULL, sum DOUBLE PRECISION NOT NULL, sum_sq DOUBLE PRECISION NOT NULL,
		 min_val DOUBLE PRECISION NOT NULL, max_val DOUBLE PRECISION NOT NULL, first_val DOUBLE PRECISION NOT NULL,
		 first_ts BIGINT NOT NULL, last_val DOUBLE PRECISION NOT NULL, last_ts BIGINT NOT NULL,
		 digest BLOB, created_at BIGINT NOT NULL,
		 UNIQUE(metric_name, entity_id, tags_hash, resolution_nano, bucket_nano))`,
		`CREATE INDEX metric__rollups_lookup_idx ON metric_rollups (metric_name, entity_id, tags_hash, resolution_nano, bucket_nano)`,
		`CREATE INDEX metric__rollups_res_time_idx ON metric_rollups (metric_name, resolution_nano, bucket_nano)`,
	}
	for _, statement := range statements {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("create legacy SQLite schema: %v", err)
		}
	}
}

func assertSQLiteV3Schema(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	for name, wantKind := range map[string]string{
		"metric_points": "view", "metric_rollups": "view",
		"metric_series": "table", "metric_point_values": "table", "metric_rollup_values": "table",
	} {
		kind, err := sqliteObjectType(ctx, db, name)
		if err != nil || kind != wantKind {
			t.Fatalf("SQLite V3 object %s: kind=%q want=%q err=%v", name, kind, wantKind, err)
		}
	}
	var indexKind string
	if err := db.QueryRowContext(ctx,
		`SELECT type FROM sqlite_master WHERE name = ?`, "metric__series_entity_idx",
	).Scan(&indexKind); err != nil || indexKind != "index" {
		t.Fatalf("SQLite V3 series entity index: kind=%q want=index err=%v", indexKind, err)
	}
}

func sqliteRootPage(t *testing.T, ctx context.Context, db *sql.DB, name string) int {
	t.Helper()
	var page int
	if err := db.QueryRowContext(ctx, `SELECT rootpage FROM sqlite_master WHERE name = ?`, name).Scan(&page); err != nil {
		t.Fatalf("read SQLite root page for %s: %v", name, err)
	}
	return page
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}

func sqlitePragmaInt(t *testing.T, ctx context.Context, db *sql.DB, name string) int {
	t.Helper()
	var value int
	if err := db.QueryRowContext(ctx, `PRAGMA `+name).Scan(&value); err != nil {
		t.Fatalf("read SQLite PRAGMA %s: %v", name, err)
	}
	return value
}

func sqliteQueryPlan(t *testing.T, ctx context.Context, db *sql.DB, query string, args ...any) []string {
	t.Helper()
	rows, err := db.QueryContext(ctx, `EXPLAIN QUERY PLAN `+query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	return plan
}

func assertSQLiteQueryPlanUses(t *testing.T, plan []string, fragments ...string) {
	t.Helper()
	joined := strings.Join(plan, " | ")
	for _, fragment := range fragments {
		if !strings.Contains(joined, fragment) {
			t.Fatalf("SQLite query plan %q does not use %q", joined, fragment)
		}
	}
}
