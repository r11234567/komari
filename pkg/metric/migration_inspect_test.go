package metric

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestSQLiteMigrationInspectorLeavesEmptyDatabaseOnNormalStartup(t *testing.T) {
	ctx := context.Background()
	dsn := sqliteFileDSN(filepath.Join(t.TempDir(), "metrics.db"))
	summary, err := InspectSQLiteMigration(ctx, SQLite(dsn))
	if err != nil {
		t.Fatal(err)
	}
	if summary.Required || summary.Layout != "empty" {
		t.Fatalf("new database should not enter migration mode: %#v", summary)
	}

	store, err := Open(ctx, SQLite(dsn))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", sqliteFilePath(dsn))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var userVersion int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&userVersion); err != nil {
		t.Fatal(err)
	}
	if userVersion != sqliteStorageVersionCurrent {
		t.Fatalf("new V4 database marker=%d want %d", userVersion, sqliteStorageVersionCurrent)
	}
}
