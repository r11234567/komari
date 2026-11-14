package metric

import (
	"context"
	"database/sql"
)

// Retire the old payload/identity SHA ledger. Keep only pending handoff state;
// the natural transport identity handles retries while that input is queued.
func (t *Telemetry) simplifyReceiptSchema(ctx context.Context) error {
	rows, err := t.s.db.QueryContext(ctx, "SELECT identity,importing FROM "+t.table("receipts")+" LIMIT 0")
	if err == nil {
		rows.Close()
		return nil
	}
	tx, err := t.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	name := t.table("receipts_pending")
	primary := "BIGINT PRIMARY KEY"
	if t.s.cfg.Driver == DriverSQLite {
		primary = "INTEGER PRIMARY KEY"
	}
	_, err = tx.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS "+name+" (ingest_id "+primary+",source VARCHAR(255) NOT NULL,identity VARCHAR(256) NOT NULL,observed BIGINT NOT NULL,received BIGINT NOT NULL,points INTEGER NOT NULL,segment_id CHAR(32) NOT NULL,importing INTEGER NOT NULL DEFAULT 0,UNIQUE(source,identity))")
	if err != nil {
		return err
	}
	// An interrupted legacy import still owns its queued input. Old hashes are
	// opaque migration retry IDs only, never recomputed for online payloads.
	flag := "CASE WHEN EXISTS (SELECT 1 FROM " + t.table("progress") + " WHERE task='legacy_migration_started' AND value=1) AND NOT EXISTS (SELECT 1 FROM " + t.table("progress") + " WHERE task='legacy_migration_complete' AND value=1) THEN 1 ELSE 0 END"
	statement := "INSERT INTO " + name + " (ingest_id,source,identity,observed,received,points,segment_id,importing) SELECT ingest_id,source,identity_hash,observed,received,points,segment_id," + flag + " FROM " + t.table("receipts")
	if t.s.cfg.Driver == DriverMySQL {
		statement += " ON DUPLICATE KEY UPDATE ingest_id=VALUES(ingest_id)"
	}
	if _, err = tx.ExecContext(ctx, statement); err != nil {
		return err
	}
	if t.s.cfg.Driver == DriverMySQL {
		old := t.table("receipts_retired")
		if _, err = tx.ExecContext(ctx, "RENAME TABLE "+t.table("receipts")+" TO "+old+", "+name+" TO "+t.table("receipts")); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "DROP TABLE "+old)
	} else {
		if _, err = tx.ExecContext(ctx, "DROP TABLE "+t.table("receipts")); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "ALTER TABLE "+name+" RENAME TO "+t.table("receipts"))
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

// Compatibility for a pre-v9 offline import interrupted before its window
// checkpoint. This runs only when that importer supplies the original ID.
func (t *Telemetry) legacyImportReceipt(ctx context.Context, tx *sql.Tx, source, identity string, result *TelemetryReceipt) error {
	hash := telemetryMigrationKey(source + "\x00" + identity)
	return tx.QueryRowContext(ctx, t.bind("SELECT ingest_id,points FROM "+t.table("receipts")+" WHERE source=? AND identity=? AND importing=1"), source, hash).Scan(&result.IngestID, &result.Points)
}
