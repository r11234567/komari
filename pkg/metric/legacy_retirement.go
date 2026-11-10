package metric

import (
	"context"
	"fmt"
)

// openRetiredSQLiteStorage performs schema-only preparation. Unlike the old
// startup path, it never seals points, rewrites axes/digests or rebuilds tiers.
// Old files remain authoritative until the explicit resumable migration.
func (s *Store) openRetiredSQLiteStorage(ctx context.Context) error {
	pointKind, err := sqliteObjectType(ctx, s.db, s.tables.points)
	if err != nil {
		return err
	}
	rollupKind, err := sqliteObjectType(ctx, s.db, s.tables.rollups)
	if err != nil {
		return err
	}
	if pointKind == "table" && rollupKind == "table" {
		// Pre-normalization relational layouts can still be queried directly.
		// Do not turn server startup into a full-table conversion.
		return nil
	}
	fresh := pointKind == "" && rollupKind == ""
	if !fresh && (pointKind != "view" || rollupKind != "view") {
		return fmt.Errorf("metric: legacy layout requires explicit offline migration (points=%q rollups=%q)", pointKind, rollupKind)
	}
	if fresh {
		if err := s.createSQLiteStorageV3(ctx); err != nil {
			return err
		}
	} else if err := s.ensureSQLiteStorageV3(ctx); err != nil {
		return err
	}
	s.sqliteStorageV3 = true
	// Empty compatibility tables let the existing bounded readers combine old
	// blocks and new hot rows. Creating them never encodes any source sample.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
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
		return err
	}
	if err := s.ensureSQLiteV8PingColumns(ctx); err != nil {
		return err
	}
	s.sqliteStorageV4 = true
	if fresh {
		// No existing separate ping.loss series needs conversion in a fresh DB.
		if err := s.markSQLiteStorageCurrent(ctx); err != nil {
			return err
		}
	}
	var version int
	if err := s.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	s.sqlitePingMerged = version >= sqliteStorageVersionPingMerge
	return nil
}
