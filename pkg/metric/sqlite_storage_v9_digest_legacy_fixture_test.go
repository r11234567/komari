//go:build metric_legacy_fixture

package metric

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func TestSQLiteV9MigratesDigestCodecLosslessly(t *testing.T) {
	ctx := context.Background()
	dsn := sqliteFileDSN(filepath.Join(t.TempDir(), "metrics.db"))
	want := seedSQLiteV8DigestStore(t, dsn, false)
	summary, err := InspectSQLiteMigration(ctx, SQLite(dsn))
	if err != nil || !summary.Required || summary.Layout != "v4" {
		t.Fatalf("V8 digest store did not enter migration page: summary=%#v err=%v", summary, err)
	}
	var phases []string
	store, err := Open(ctx, SQLite(dsn, WithMigrationProgress(func(progress MigrationProgress) {
		phases = append(phases, progress.Phase)
	})))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if !containsString(phases, MigrationPhaseEncodingDigests) {
		t.Fatalf("digest conversion was not visible in migration progress: %v", phases)
	}
	var codec, userVersion int
	if err := store.db.QueryRowContext(ctx, `SELECT digest_codec FROM metric_rollup_blocks LIMIT 1`).Scan(&codec); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&userVersion); err != nil {
		t.Fatal(err)
	}
	if codec != sqliteV4StructuredRollupDigestCodec || userVersion != sqliteStorageVersionCurrent {
		t.Fatalf("migrated codec=%d version=%d", codec, userVersion)
	}
	series, err := store.sqliteV4MatchingSeries(ctx, store.db, "v9.digest", "node-a", map[string]string{"source": "v8"})
	if err != nil || len(series) != 1 {
		t.Fatalf("find migrated series: count=%d err=%v", len(series), err)
	}
	got, err := store.loadAllSQLiteV4RollupBlockRecords(ctx, store.db, series[0].id, time.Minute.Nanoseconds())
	if err != nil || !sqliteV4RollupRecordDataSlicesEqual(want, got) {
		t.Fatalf("V9 migration changed digest data: count=%d err=%v", len(got), err)
	}
}

func TestSQLiteV9MigrationFailureKeepsV8Blocks(t *testing.T) {
	ctx := context.Background()
	dsn := sqliteFileDSN(filepath.Join(t.TempDir(), "metrics.db"))
	seedSQLiteV8DigestStore(t, dsn, true)
	if store, err := Open(ctx, SQLite(dsn)); err == nil {
		_ = store.Close()
		t.Fatal("corrupt V8 digest unexpectedly migrated")
	}
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var codec, userVersion int
	if err := db.QueryRowContext(ctx, `SELECT digest_codec FROM metric_rollup_blocks LIMIT 1`).Scan(&codec); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&userVersion); err != nil {
		t.Fatal(err)
	}
	if codec != sqliteV4LegacyStructuredDigestCodec || userVersion != sqliteStorageVersionPingMerge {
		t.Fatalf("failed migration changed source codec=%d version=%d", codec, userVersion)
	}
}
