package metric

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

type LegacyMigrationState struct {
	Cutoff         time.Time
	LedgerComplete bool
	Complete       bool
}

// PrepareLegacyMigration fixes the clock before either database is modified.
// Both automatic startup and the offline CLI use this durable checkpoint.
func (s *Store) PrepareLegacyMigration(ctx context.Context, cfg TelemetryConfig) (state LegacyMigrationState, err error) {
	if s.Telemetry() != nil {
		return state, errors.New("migration requires an offline legacy reader")
	}
	t, err := s.OpenTelemetry(ctx, cfg)
	if err != nil {
		return state, err
	}
	defer t.Close()
	tx, err := t.begin(ctx)
	if err != nil {
		return state, err
	}
	defer tx.Rollback()
	get := func(key string) (int64, error) {
		v, e := t.progress(ctx, tx, key)
		if errors.Is(e, sql.ErrNoRows) {
			return 0, nil
		}
		return v, e
	}
	done, err := get("legacy_migration_complete")
	if err != nil {
		return state, err
	}
	ledger, err := get("legacy_ledger_complete")
	if err != nil {
		return state, err
	}
	epoch, err := get("legacy_migration_now")
	if err != nil {
		return state, err
	}
	state.Complete, state.LedgerComplete = done == 1, ledger == 1
	if state.Complete {
		state.Cutoff = time.Unix(0, epoch).UTC()
		return state, nil
	}
	accepted, err := get("ingest")
	if err != nil {
		return state, err
	}
	started, err := get("legacy_migration_started")
	if err != nil {
		return state, err
	}
	if accepted > 0 && started != 1 {
		return state, errors.New("target already contains online telemetry; refusing an ambiguous offline merge")
	}
	if epoch == 0 {
		epoch = time.Now().UTC().UnixNano()
	}
	if err = t.setProgress(ctx, tx, "legacy_migration_now", epoch); err != nil {
		return state, err
	}
	if err = t.setProgress(ctx, tx, "legacy_migration_started", 1); err != nil {
		return state, err
	}
	state.Cutoff = time.Unix(0, epoch).UTC()
	return state, tx.Commit()
}

func (s *Store) CompleteLegacyLedgerMigration(ctx context.Context, cfg TelemetryConfig) error {
	t, err := s.OpenTelemetry(ctx, cfg)
	if err != nil {
		return err
	}
	defer t.Close()
	tx, err := t.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = t.setProgress(ctx, tx, "legacy_ledger_complete", 1); err != nil {
		return err
	}
	return tx.Commit()
}

func inspectRetiredTelemetryMigration(ctx context.Context, db *sql.DB, prefix string) (SQLiteMigrationSummary, error) {
	result := SQLiteMigrationSummary{Layout: "legacy-retired"}
	progress := tableName(prefix, "telemetry_progress")
	kind, err := sqliteObjectTypeDB(ctx, db, progress)
	if err != nil {
		return result, err
	}
	if kind == "table" {
		var done, started int64
		if err = db.QueryRowContext(ctx, "SELECT COALESCE(MAX(CASE WHEN task='legacy_migration_complete' THEN value END),0),COALESCE(MAX(CASE WHEN task='legacy_migration_started' THEN value END),0) FROM "+progress+" WHERE task IN ('legacy_migration_complete','legacy_migration_started')").Scan(&done, &started); err != nil {
			return result, err
		}
		if done == 1 {
			return result, nil
		}
		if started == 1 {
			result.Required, result.Layout = true, "telemetry-resume"
			return result, nil
		}
	}
	for _, suffix := range []string{"points", "rollups", "point_values", "point_blocks", "rollup_values", "rollup_blocks"} {
		table := tableName(prefix, suffix)
		kind, err := sqliteObjectTypeDB(ctx, db, table)
		if err != nil {
			return result, err
		}
		if kind != "table" {
			continue
		}
		var one int
		err = db.QueryRowContext(ctx, "SELECT 1 FROM "+table+" LIMIT 1").Scan(&one)
		if err == nil {
			result.Required, result.Layout = true, "telemetry-legacy"
			return result, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return result, err
		}
	}
	return result, nil
}
