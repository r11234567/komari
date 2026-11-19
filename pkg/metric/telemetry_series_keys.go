package metric

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
)

// Keep the complete canonical hash once in a dictionary. SQLite references use
// a short, stable key instead of repeating 64 hex bytes in every index entry.
// Existing long keys remain valid until an explicit offline rewrite.
func (t *Telemetry) seriesKey(ctx context.Context, tx *sql.Tx, canonical string) (string, error) {
	if t.s.cfg.Driver != DriverSQLite {
		return canonical, nil
	}
	var id int64
	err := tx.QueryRowContext(ctx, "SELECT id FROM "+t.table("series_keys")+" WHERE canonical=?", canonical).Scan(&id)
	if err == nil {
		return "@" + strconv.FormatInt(id, 10), nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	var old string
	err = tx.QueryRowContext(ctx, "SELECT id FROM "+t.table("series")+" WHERE id=?", canonical).Scan(&old)
	if err == nil {
		return old, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	r, err := tx.ExecContext(ctx, "INSERT INTO "+t.table("series_keys")+" (canonical) VALUES (?)", canonical)
	if err != nil {
		return "", err
	}
	id, err = r.LastInsertId()
	return "@" + strconv.FormatInt(id, 10), err
}

func (t *Telemetry) canonicalSeriesSQL(alias string) string {
	if t.s.cfg.Driver != DriverSQLite {
		return alias + ".id"
	}
	return "COALESCE((SELECT k.canonical FROM " + t.table("series_keys") + " k WHERE k.id=CASE WHEN substr(" + alias + ".id,1,1)='@' THEN CAST(substr(" + alias + ".id,2) AS INTEGER) END)," + alias + ".id)"
}
