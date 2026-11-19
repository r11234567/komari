package metric

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// HasTelemetrySchema fences legacy raw-only copying even when the caller has
// opened the SQL file without activating its archive reader.
func (s *Store) HasTelemetrySchema(ctx context.Context) (bool, error) {
	name := s.cfg.TablePrefix + "telemetry_progress"
	var n int
	query := "SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?"
	switch s.cfg.Driver {
	case DriverPostgreSQL:
		query = "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=current_schema() AND table_name=$1"
	case DriverMySQL:
		query = "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name=?"
	}
	err := s.reader().QueryRowContext(ctx, query, name).Scan(&n)
	return n > 0, err
}

// RequireTelemetryMigration makes cutover explicit. Indexed existence probes
// inspect each physical source format; no COUNT or historical decode is run.
func (s *Store) RequireTelemetryMigration(ctx context.Context) error {
	var done int
	err := s.reader().QueryRowContext(ctx, "SELECT value FROM "+s.cfg.TablePrefix+"telemetry_progress WHERE task='legacy_migration_complete'").Scan(&done)
	if err == nil && done == 1 {
		return nil
	}
	table, err := s.legacyTelemetryInput(ctx, s.reader())
	if err != nil {
		return err
	}
	if table != "" {
		return fmt.Errorf("legacy telemetry exists in %s; stop the service and run telemetry-migrate before cutover; the original source is unchanged", table)
	}
	return nil
}

func (s *Store) legacyTelemetryInput(ctx context.Context, reader querier) (string, error) {
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
		err := reader.QueryRowContext(ctx, "SELECT 1 FROM "+table+" LIMIT 1").Scan(&one)
		if err == nil {
			return table, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
	}
	return "", nil
}
