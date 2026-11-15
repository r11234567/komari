package metric

import (
	"context"
	"testing"
)

func createSQLiteV3OnlyStore(t *testing.T, ctx context.Context, dsn string) *Store {
	t.Helper()
	store, err := Open(ctx, SQLite(dsn, WithAutoMigrate(false)))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.migrateSQLiteStorageV3(ctx, true); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	return store
}
