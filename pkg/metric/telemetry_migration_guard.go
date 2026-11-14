package metric

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// RequireTelemetryMigration makes cutover explicit. Indexed existence probes
// inspect each physical source format; no COUNT or historical decode is run.
func (s *Store) RequireTelemetryMigration(ctx context.Context) error {
	var done int
	err := s.reader().QueryRowContext(ctx, "SELECT value FROM "+s.cfg.TablePrefix+"telemetry_progress WHERE task='legacy_migration_complete'").Scan(&done)
	if err == nil && done == 1 {
		return nil
	}
	tables := []string{s.tables.points, s.tables.rollups}
	if s.sqliteStorageV4 {
		tables = []string{s.tables.pointValues, s.tables.pointBlocks, s.tables.rollupValues, s.tables.rollupBlocks}
	} else if s.externalPointBlocks {
		tables = append(tables, s.tables.pointBlocks)
	}
	for _, table := range tables {
		if table == "" {
			continue
		}
		var one int
		err = s.reader().QueryRowContext(ctx, "SELECT 1 FROM "+table+" LIMIT 1").Scan(&one)
		if err == nil {
			return fmt.Errorf("legacy telemetry exists in %s; stop the service and run telemetry-migrate before cutover; the original source is unchanged", table)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	return nil
}
