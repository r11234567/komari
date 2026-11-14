package metric

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// TelemetryCompactResult records the independent rollback material and the
// physically compacted replacement. The caller installs the replacement only
// while all panel/worker processes are stopped. No source file is exchanged.
type TelemetryCompactResult struct {
	Database          string    `json:"database"`
	Backup            string    `json:"backup"`
	RetainBackupUntil time.Time `json:"retain_backup_until"`
	Bytes             int64     `json:"bytes"`
	SHA256            string    `json:"sha256"`
	StateSHA256       string    `json:"state_sha256"`
	RetiredLegacy     bool      `json:"retired_legacy"`
}

// CompactSQLite is an explicit offline operation, never scheduled or exposed
// over HTTP. It creates a verified FULL backup (including immutable archives),
// rewrites only a disposable copy, checks logical state, then publishes a new
// standalone SQL file with O_EXCL semantics. Failure leaves the source intact.
// metadata should snapshot the application's metadata DB into backupDirectory.
func (t *Telemetry) CompactSQLite(ctx context.Context, destination, backupDirectory string, retire bool, metadata func() error) (result TelemetryCompactResult, err error) {
	if t.s.cfg.Driver != DriverSQLite {
		return result, fmt.Errorf("offline compaction requires SQLite")
	}
	if destination == "" || backupDirectory == "" {
		return result, ErrInvalidArgument
	}
	if _, err = t.s.db.ExecContext(ctx, "PRAGMA temp_store=FILE"); err != nil {
		return result, err
	}
	if _, err = os.Lstat(destination); !os.IsNotExist(err) {
		return result, fmt.Errorf("compaction destination must not exist")
	}
	if err = os.Mkdir(backupDirectory, 0700); err != nil {
		return result, fmt.Errorf("create new rollback directory: %w", err)
	}
	backup := filepath.Join(backupDirectory, "metrics.db")
	archives := filepath.Join(backupDirectory, "telemetry")
	if err = t.BackupBundle(ctx, backup, archives, metadata); err != nil {
		return result, err
	}
	if err = ValidateTelemetryBackup(ctx, backup, archives); err != nil {
		return result, fmt.Errorf("verify rollback bundle: %w", err)
	}
	// Backup files were individually fsynced. Persist every containing
	// directory before it is accepted as rollback material.
	if err = filepath.WalkDir(backupDirectory, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() {
			return nil
		}
		d, e := os.Open(path)
		if e != nil {
			return e
		}
		defer d.Close()
		return d.Sync()
	}); err != nil {
		return result, err
	}
	if err = t.fault("compact-backup-verified"); err != nil {
		return result, err
	}

	// Freeze SQL writers, including other processes, after making the backup.
	// Comparing every durable telemetry row detects a write between snapshot
	// creation and this fence. No maintenance can expire a catalogued archive
	// while this writer transaction is held.
	release, err := t.acquire(ctx)
	if err != nil {
		return result, err
	}
	defer release()
	fence, err := t.begin(ctx)
	if err != nil {
		return result, err
	}
	defer fence.Rollback()
	sourceHash, err := t.compactStateHash(ctx, fence)
	if err != nil {
		return result, err
	}
	if err = os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		return result, err
	}
	dir, err := os.MkdirTemp(filepath.Dir(destination), ".telemetry-compact-*")
	if err != nil {
		return result, err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "metrics.db")
	db, err := sql.Open("sqlite3", setSQLiteDSNParam(sqliteFileDSN(backup), "mode", "ro"))
	if err != nil {
		return result, err
	}
	// VACUUM INTO produces a consistent standalone copy, including committed
	// WAL pages; neither the source nor the retained backup is vacuumed in place.
	_, err = db.ExecContext(ctx, "VACUUM INTO '"+strings.ReplaceAll(filepath.ToSlash(path), "'", "''")+"'")
	closeErr := db.Close()
	if err != nil {
		return result, err
	}
	if closeErr != nil {
		return result, closeErr
	}
	db, err = sql.Open("sqlite3", sqliteFileDSN(path))
	if err != nil {
		return result, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, q := range []string{"PRAGMA synchronous=FULL", "PRAGMA temp_store=FILE", "PRAGMA cache_size=-2048"} {
		if _, err = db.ExecContext(ctx, q); err != nil {
			return result, err
		}
	}
	beforeHash, err := t.compactStateHash(ctx, db)
	if err != nil {
		return result, err
	}
	if beforeHash != sourceHash {
		return result, errors.New("source changed during backup; stop all panel/worker processes and retry with a new output/backup directory")
	}
	if retire {
		var migrated, ledger int
		err = db.QueryRowContext(ctx, "SELECT COALESCE(MAX(CASE WHEN task='legacy_migration_complete' THEN value END),0),COALESCE(MAX(CASE WHEN task='legacy_ledger_complete' THEN value END),0) FROM "+t.table("progress")).Scan(&migrated, &ledger)
		if err != nil {
			return result, err
		}
		if migrated != 1 || ledger != 1 {
			return result, errors.New("legacy retirement requires completed history AND business-ledger migration")
		}
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	// A replacement file has no running export/backup owner. Snapshot pins and
	// worker leases belong to the source process, not to its offline copy.
	for _, suffix := range []string{"leases", "work"} {
		if _, err = tx.ExecContext(ctx, "DELETE FROM "+t.table(suffix)); err != nil {
			return result, err
		}
	}
	if _, err = tx.ExecContext(ctx, "UPDATE "+t.table("schedule")+" SET owner='',expires=0"); err != nil {
		return result, err
	}
	if err = t.compactSeriesKeys(ctx, tx); err != nil {
		return result, err
	}
	for _, idx := range []struct{ name, columns string }{
		{"summary_series", "series_id"}, {"summary_age", "resolution,bucket"}, {"summary_pending", "resolution,promoted_through,bucket"},
	} {
		name := t.s.cfg.TablePrefix + "telemetry_" + idx.name
		if _, err = tx.ExecContext(ctx, "DROP INDEX IF EXISTS "+name); err != nil {
			return result, err
		}
		if _, err = tx.ExecContext(ctx, "CREATE INDEX "+name+" ON "+t.table("summary")+" ("+idx.columns+")"); err != nil {
			return result, err
		}
	}
	if retire {
		// Restrict removal to the known legacy schema. Definitions, unrelated
		// application tables and all telemetry/traffic state are preserved.
		for _, suffix := range []string{"points", "rollups", "point_values", "rollup_values", "point_blocks", "rollup_blocks", "point_axes", "rollup_axes", "labels", "resolutions", "series", "compaction_watermarks"} {
			name := tableName(t.s.cfg.TablePrefix, suffix)
			var kind string
			err = tx.QueryRowContext(ctx, "SELECT type FROM sqlite_master WHERE name=? AND type IN ('table','view')", name).Scan(&kind)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return result, err
			}
			if _, err = tx.ExecContext(ctx, "DROP "+strings.ToUpper(kind)+" "+name); err != nil {
				return result, err
			}
		}
		if err = t.setProgress(ctx, tx, "legacy_tables_retired", 1); err != nil {
			return result, err
		}
	}
	// Queued exports from the previous file cannot reuse its input generation.
	if _, err = tx.ExecContext(ctx, "UPDATE "+t.table("progress")+" SET value=value+1 WHERE task='export_epoch'"); err != nil {
		return result, err
	}
	if err = t.fault("compact-before-commit"); err != nil {
		return result, err
	}
	if err = tx.Commit(); err != nil {
		return result, err
	}
	if _, err = db.ExecContext(ctx, "VACUUM"); err != nil {
		return result, err
	}
	var check string
	if err = db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&check); err != nil {
		return result, err
	}
	if check != "ok" {
		return result, fmt.Errorf("compacted SQLite integrity: %s", check)
	}
	afterHash, err := t.compactStateHash(ctx, db)
	if err != nil {
		return result, err
	}
	if afterHash != beforeHash {
		return result, errors.New("compacted telemetry logical state mismatch")
	}
	if err = db.Close(); err != nil {
		return result, err
	}
	if err = t.fault("compact-before-publish"); err != nil {
		return result, err
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return result, err
	}
	if err = f.Chmod(0600); err != nil {
		f.Close()
		return result, err
	}
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err == nil {
		err = f.Sync()
	}
	f.Close()
	if err != nil {
		return result, err
	}
	result = TelemetryCompactResult{Database: destination, Backup: backupDirectory, RetainBackupUntil: time.Now().UTC().Add(7 * 24 * time.Hour), Bytes: n, SHA256: hex.EncodeToString(h.Sum(nil)), StateSHA256: afterHash, RetiredLegacy: retire}
	// Persist the verification record before publishing. On an interrupted
	// publish the record may exist without an output; it never authorizes a swap.
	b, _ := json.MarshalIndent(result, "", "  ")
	mf, err := os.OpenFile(filepath.Join(backupDirectory, "compaction.json"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return result, err
	}
	if _, err = mf.Write(append(b, '\n')); err == nil {
		err = mf.Sync()
	}
	mf.Close()
	if err != nil {
		return result, err
	}
	bd, err := os.Open(backupDirectory)
	if err != nil {
		return result, err
	}
	err = bd.Sync()
	bd.Close()
	if err != nil {
		return result, err
	}
	if err = os.Link(path, destination); err != nil {
		return result, err
	}
	d, err := os.Open(filepath.Dir(destination))
	if err != nil {
		return result, err
	}
	defer d.Close()
	return result, d.Sync()
}

func (t *Telemetry) compactSeriesKeys(ctx context.Context, tx *sql.Tx) error {
	keys := t.table("series_keys")
	// A full hash is retained once; never truncate hashes or change canonical
	// identity. Existing @ keys must resolve before any rewrite is attempted.
	var invalid int
	for _, ref := range []struct{ table, column string }{{"series", "id"}, {"hot", "series_id"}, {"latest", "series_id"}, {"summary", "series_id"}, {"members", "series_id"}, {"erasures", "series_id"}, {"gaps", "series_id"}, {"rollup_keys", "source_id"}} {
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+t.table(ref.table)+" s WHERE substr(s."+ref.column+",1,1)='@' AND NOT EXISTS (SELECT 1 FROM "+keys+" k WHERE k.id=CAST(substr(s."+ref.column+",2) AS INTEGER) AND '@'||k.id=s."+ref.column+")").Scan(&invalid); err != nil {
			return err
		}
		if invalid != 0 {
			return errors.New("unresolved telemetry series dictionary key")
		}
	}
	if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO "+keys+" (canonical) SELECT id FROM "+t.table("series")+" WHERE substr(id,1,1)<>'@' UNION SELECT series_id FROM "+t.table("erasures")+" WHERE substr(series_id,1,1)<>'@' ORDER BY 1"); err != nil {
		return err
	}
	for _, table := range []string{"hot", "latest", "summary", "members", "erasures", "gaps"} {
		if _, err := tx.ExecContext(ctx, "UPDATE "+t.table(table)+" SET series_id=(SELECT '@'||k.id FROM "+keys+" k WHERE k.canonical=series_id) WHERE substr(series_id,1,1)<>'@'"); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, "UPDATE "+t.table("rollup_keys")+" SET source_id=(SELECT '@'||k.id FROM "+keys+" k WHERE k.canonical=source_id) WHERE substr(source_id,1,1)<>'@'"); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, "UPDATE "+t.table("series")+" SET id=(SELECT '@'||k.id FROM "+keys+" k WHERE k.canonical="+t.table("series")+".id) WHERE substr(id,1,1)<>'@'")
	return err
}

// Stable typed row encoding, canonicalizing series references before ordering
// by the table primary key. Streams through SQLite's disk-backed sort; neither
// decoded sketches nor an entire table are accumulated in Go memory.
func (t *Telemetry) compactStateHash(ctx context.Context, db querier) (string, error) {
	h := sha256.New()
	put := func(v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		var size [8]byte
		binary.LittleEndian.PutUint64(size[:], uint64(len(b)))
		h.Write(size[:])
		h.Write(b)
		return nil
	}
	for _, suffix := range []string{"definitions", "receipts", "series", "hot", "latest", "segments", "summary", "rollup_keys", "rollup", "rollup_due", "raw_gaps", "outbox", "members", "erasures", "identity", "traffic_ingress", "gaps", "progress"} {
		name := t.table(suffix)
		if suffix == "definitions" {
			name = t.s.tables.definitions
		}
		info, err := db.QueryContext(ctx, "PRAGMA table_info("+name+")")
		if err != nil {
			return "", err
		}
		var columns []string
		order := make(map[int]int)
		for info.Next() {
			var cid, required, pk int
			var column, typ string
			var def any
			if err = info.Scan(&cid, &column, &typ, &required, &def, &pk); err != nil {
				break
			}
			if pk > 0 {
				order[pk] = len(columns) + 1
			}
			if (column == "series_id" && suffix != "rollup" && suffix != "rollup_due" && suffix != "raw_gaps") || (suffix == "series" && column == "id") || (suffix == "rollup_keys" && column == "source_id") {
				column = "COALESCE((SELECT canonical FROM " + t.table("series_keys") + " k WHERE k.id=CASE WHEN substr(r." + column + ",1,1)='@' THEN CAST(substr(r." + column + ",2) AS INTEGER) END),r." + column + ")"
			} else {
				column = "r." + column
			}
			columns = append(columns, column)
		}
		re := info.Err()
		info.Close()
		if err != nil {
			return "", err
		}
		if re != nil {
			return "", re
		}
		if len(columns) == 0 {
			return "", fmt.Errorf("missing compact source table %s", name)
		}
		var sortColumns []string
		for i := 1; i <= len(order); i++ {
			sortColumns = append(sortColumns, fmt.Sprint(order[i]))
		}
		where := ""
		if suffix == "progress" {
			where = " WHERE task NOT IN ('legacy_tables_retired','export_epoch')"
		}
		rows, err := db.QueryContext(ctx, "SELECT "+strings.Join(columns, ",")+" FROM "+name+" r"+where+" ORDER BY "+strings.Join(sortColumns, ","))
		if err != nil {
			return "", err
		}
		if err = put(suffix); err != nil {
			rows.Close()
			return "", err
		}
		for rows.Next() {
			values := make([]any, len(columns))
			ptrs := make([]any, len(columns))
			for i := range values {
				ptrs[i] = &values[i]
			}
			if err = rows.Scan(ptrs...); err != nil {
				break
			}
			if err = put(values); err != nil {
				break
			}
		}
		re = rows.Err()
		rows.Close()
		if err != nil {
			return "", err
		}
		if re != nil {
			return "", re
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
