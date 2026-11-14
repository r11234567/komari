//go:build metric_legacy_fixture

package metric

import (
	"context"
	"fmt"
	"log"
	"math"
	"time"
)

// migrateSQLiteStorageV4 first brings legacy layouts to the normalized V3
// schema, then atomically moves raw values and long-term rollups into verified
// lossless blocks. Databases created by the first V4 test build have point
// blocks but no rollup blocks; they are upgraded in place without re-encoding
// the already migrated points.
func (s *Store) migrateSQLiteStorageV4(ctx context.Context) error {
	s.reportMigrationProgress(MigrationPhasePreparing, 0, 1, 0)
	// V3 is an internal normalization step here. Physical space reclamation is
	// intentionally left to the explicit database maintenance action.
	if err := s.migrateSQLiteStorageV3(ctx, false); err != nil {
		return err
	}
	pointKind, err := sqliteObjectType(ctx, s.db, s.tables.pointBlocks)
	if err != nil {
		return err
	}
	rollupKind, err := sqliteObjectType(ctx, s.db, s.tables.rollupBlocks)
	if err != nil {
		return err
	}
	if pointKind != "" && pointKind != "table" {
		return fmt.Errorf("metric: SQLite V4 object %s has unexpected type %q", s.tables.pointBlocks, pointKind)
	}
	if rollupKind != "" && rollupKind != "table" {
		return fmt.Errorf("metric: SQLite V4 object %s has unexpected type %q", s.tables.rollupBlocks, rollupKind)
	}
	if pointKind == "table" && rollupKind == "table" {
		s.sqliteStorageV4 = true
		if err := s.ensureSQLiteStorageV4(ctx); err != nil {
			return err
		}
		if err := s.markSQLiteStorageCurrent(ctx); err != nil {
			return err
		}
		// Do not run PRAGMA optimize on every server start. On a large raw-history
		// database SQLite may choose to analyze the complete hot table here, which
		// turns an otherwise idle restart into sustained CPU and disk I/O.
		// Fresh schema migrations still optimize below, after their one-off work.
		s.reportMigrationProgress(MigrationPhaseCompleted, 1, 1, 0)
		return nil
	}

	log.Printf("metric: migrating SQLite metric storage to V%d", sqliteStorageVersionV4)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("metric: begin SQLite V4 migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.createSQLiteV4PointBlocks(ctx, tx); err != nil {
		return err
	}
	if err := s.createSQLiteV4RollupBlocks(ctx, tx); err != nil {
		return err
	}
	if err := s.validateSQLiteV4PointBlocks(ctx, tx); err != nil {
		return err
	}
	if err := s.validateSQLiteV4RollupBlocks(ctx, tx); err != nil {
		return err
	}
	var pointSourceCount, rollupSourceCount int64
	if pointKind == "" {
		pointSourceCount, err = sqliteTableRowCountTx(ctx, tx, s.tables.pointValues)
		if err != nil {
			return err
		}
		s.reportMigrationProgress(MigrationPhaseEncodingPoints, 0, pointSourceCount, 0)
		sealed, err := s.sealSQLiteV4PointsTx(ctx, tx, "", math.MaxInt64, pointSourceCount, 0)
		if err != nil {
			return fmt.Errorf("metric: encode SQLite V4 point blocks: %w", err)
		}
		if sealed != pointSourceCount {
			return fmt.Errorf("metric: SQLite V4 point count mismatch: source=%d encoded=%d", pointSourceCount, sealed)
		}
		s.reportMigrationProgress(MigrationPhaseEncodingPoints, sealed, pointSourceCount, sealed)
	}
	if rollupKind == "" {
		rollupSourceCount, err = sqliteTableRowCountTx(ctx, tx, s.tables.rollupValues)
		if err != nil {
			return err
		}
		s.reportMigrationProgress(MigrationPhaseEncodingRollups, 0, rollupSourceCount, pointSourceCount)
		sealed, err := s.sealAllSQLiteV4RollupsTx(ctx, tx, rollupSourceCount, pointSourceCount)
		if err != nil {
			return fmt.Errorf("metric: encode SQLite V4 rollup blocks: %w", err)
		}
		if sealed != rollupSourceCount {
			return fmt.Errorf("metric: SQLite V4 rollup count mismatch: source=%d encoded=%d", rollupSourceCount, sealed)
		}
		s.reportMigrationProgress(MigrationPhaseEncodingRollups, sealed, rollupSourceCount, pointSourceCount+sealed)
	}
	s.reportMigrationProgress(MigrationPhaseValidating, 0, pointSourceCount+rollupSourceCount, pointSourceCount+rollupSourceCount)
	var pointHotCount, pointBlockCount, rollupHotCount, rollupBlockCount int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+s.tables.pointValues).Scan(&pointHotCount); err != nil {
		return fmt.Errorf("metric: validate SQLite V4 hot points: %w", err)
	}
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(point_count), 0) FROM `+s.tables.pointBlocks).Scan(&pointBlockCount); err != nil {
		return fmt.Errorf("metric: validate SQLite V4 point blocks: %w", err)
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+s.tables.rollupValues).Scan(&rollupHotCount); err != nil {
		return fmt.Errorf("metric: validate SQLite V4 hot rollups: %w", err)
	}
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(bucket_count), 0) FROM `+s.tables.rollupBlocks).Scan(&rollupBlockCount); err != nil {
		return fmt.Errorf("metric: validate SQLite V4 rollup blocks: %w", err)
	}
	if pointKind == "" && (pointHotCount != 0 || pointBlockCount != pointSourceCount) {
		return fmt.Errorf("metric: SQLite V4 point validation failed: source=%d hot=%d blocks=%d", pointSourceCount, pointHotCount, pointBlockCount)
	}
	if rollupKind == "" && (rollupHotCount != 0 || rollupBlockCount != rollupSourceCount) {
		return fmt.Errorf("metric: SQLite V4 rollup validation failed: source=%d hot=%d blocks=%d", rollupSourceCount, rollupHotCount, rollupBlockCount)
	}
	s.reportMigrationProgress(MigrationPhaseValidating, pointSourceCount+rollupSourceCount, pointSourceCount+rollupSourceCount, pointSourceCount+rollupSourceCount)
	s.reportMigrationProgress(MigrationPhaseCommitting, 0, 1, pointSourceCount+rollupSourceCount)
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("metric: commit SQLite V4 migration: %w", err)
	}
	s.reportMigrationProgress(MigrationPhaseCommitting, 1, 1, pointSourceCount+rollupSourceCount)
	s.sqliteStorageV4 = true
	if err := s.ensureSQLiteV8PingColumns(ctx); err != nil {
		return err
	}
	tierBuckets, err := s.migrateSQLiteV7TierHandoff(ctx, time.Now().UTC())
	if err != nil {
		return err
	}
	pingBuckets, err := s.migrateSQLiteV8PingSeries(ctx)
	if err != nil {
		return err
	}
	_, sharedBuckets, skippedBlocks, err := s.migrateSQLiteV4SharedRollupBlocks(ctx)
	if err != nil {
		return err
	}
	if skippedBlocks > 0 {
		log.Printf("metric: deferred %d readable rollup blocks that could not yet be converted to shared-axis storage", skippedBlocks)
	}
	if err := s.markSQLiteStorageCurrent(ctx); err != nil {
		return err
	}
	if err := optimizeSQLite(ctx, s.db); err != nil {
		log.Printf("metric: SQLite post-migration query planner optimization skipped: %v", err)
	}
	s.reportMigrationProgress(MigrationPhaseCompleted, 1, 1, pointSourceCount+rollupSourceCount+tierBuckets+pingBuckets+sharedBuckets)
	log.Printf("metric: migrated SQLite metric storage to V%d (%d raw points and %d rollups preserved bit-for-bit)", sqliteStorageVersionV4, pointSourceCount, rollupSourceCount)
	return nil
}

func (s *Store) ensureSQLiteStorageV4(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("metric: begin SQLite V4 verification: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.createSQLiteV4PointBlocks(ctx, tx); err != nil {
		return err
	}
	if err := s.createSQLiteV4RollupBlocks(ctx, tx); err != nil {
		return err
	}
	if err := s.validateSQLiteV4PointBlocks(ctx, tx); err != nil {
		return err
	}
	if err := s.validateSQLiteV4RollupBlocks(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("metric: commit SQLite V4 verification: %w", err)
	}
	if err := s.ensureSQLiteV8PingColumns(ctx); err != nil {
		return err
	}
	tierBuckets, err := s.migrateSQLiteV7TierHandoff(ctx, time.Now().UTC())
	if err != nil {
		return err
	}
	pingBuckets, err := s.migrateSQLiteV8PingSeries(ctx)
	if err != nil {
		return err
	}
	sharedBlocks, sharedBuckets, skippedBlocks, err := s.migrateSQLiteV4SharedRollupBlocks(ctx)
	if err != nil {
		return err
	}
	if skippedBlocks > 0 {
		log.Printf("metric: deferred %d readable rollup blocks that could not yet be converted to shared-axis storage", skippedBlocks)
	}
	pointAxisBlocks, pointAxisPoints, err := s.migrateSQLiteV6SharedPointBlocks(ctx)
	if err != nil {
		return err
	}
	if tierBuckets > 0 || pingBuckets > 0 || sharedBlocks > 0 || pointAxisBlocks > 0 {
		log.Printf("metric: migrated %d tier-handoff buckets, %d shared-axis rollup blocks (%d buckets), and %d shared-axis point blocks (%d points); physical space reclamation is deferred", tierBuckets, sharedBlocks, sharedBuckets, pointAxisBlocks, pointAxisPoints)
	}
	return nil
}
