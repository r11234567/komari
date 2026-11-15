//go:build metric_legacy_fixture

package metric

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestSQLiteStorageMigratesUpstream131RollupsToV4(t *testing.T) {
	ctx := context.Background()
	dsn := sqliteFileDSN(filepath.Join(t.TempDir(), "metrics.db"))
	base := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Minute)
	seedUpstream131SQLiteStore(t, dsn, base, false)

	summary, err := InspectSQLiteMigration(ctx, SQLite(dsn))
	if err != nil {
		t.Fatalf("inspect upstream 1.3.1 database: %v", err)
	}
	if !summary.Required || summary.Layout != "upstream-1.3.1" || summary.SourceRows != 2 {
		t.Fatalf("upstream 1.3.1 should enter the migration page: %#v", summary)
	}

	var phases []string
	store, err := Open(ctx, SQLite(dsn, WithMigrationProgress(func(progress MigrationProgress) {
		phases = append(phases, progress.Phase)
	})))
	if err != nil {
		t.Fatalf("upgrade upstream 1.3.1 database: %v", err)
	}
	defer store.Close()

	definition, err := store.GetMetric(ctx, "compat.131")
	if err != nil {
		t.Fatalf("read migrated definition: %v", err)
	}
	if definition.RetentionDays != 17 {
		t.Fatalf("upstream 1.3.1 retention changed: got=%d want=17", definition.RetentionDays)
	}
	if !definition.CreatedAt.Equal(base) || !definition.UpdatedAt.Equal(base.Add(time.Minute)) {
		t.Fatalf("upstream 1.3.1 definition times changed: created=%v updated=%v", definition.CreatedAt, definition.UpdatedAt)
	}

	tags := map[string]string{"task_id": "7"}
	rows, err := store.scanRollupRowsBetween(ctx, "compat.131", "upstream-node", tags,
		time.Minute.Nanoseconds(), base.UnixNano(), base.UnixNano(), true)
	if err != nil {
		t.Fatalf("read migrated upstream 1.3.1 rollup: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("migrated rollup rows=%d want=1", len(rows))
	}
	bucket := rows[0].bucketData
	if bucket.count != 2 || bucket.sum != 30 || bucket.sumSq != 500 || bucket.min != 10 || bucket.max != 20 {
		t.Fatalf("migrated rollup summary changed: %#v", bucket)
	}
	if got := bucket.digest.Quantile(0.5); got < 10 || got > 20 {
		t.Fatalf("migrated digest is invalid: p50=%v", got)
	}
	if len(phases) == 0 || phases[len(phases)-1] != MigrationPhaseCompleted {
		t.Fatalf("upstream 1.3.1 migration progress did not complete: %v", phases)
	}
	foundNormalize := false
	for _, phase := range phases {
		if phase == MigrationPhaseNormalizingRollups {
			foundNormalize = true
			break
		}
	}
	if !foundNormalize {
		t.Fatalf("upstream 1.3.1 conversion was not visible in migration progress: %v", phases)
	}

	summary, err = InspectSQLiteMigration(ctx, SQLite(dsn))
	if err != nil {
		t.Fatalf("inspect migrated upstream 1.3.1 database: %v", err)
	}
	if summary.Required || summary.Layout != "current" {
		t.Fatalf("upstream 1.3.1 migration should not repeat: %#v", summary)
	}
}
