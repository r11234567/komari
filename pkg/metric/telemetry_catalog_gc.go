package metric

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// PruneCatalogChunk retains expired ownership/gap tombstones for at least the
// maximum configured chart history and admissible retry horizon. Removing a
// cold file must not turn an explicitly missing CSV into a successful empty one.
func (t *Telemetry) PruneCatalogChunk(ctx context.Context, now time.Time, limit int) (int, error) {
	if limit < 1 || limit > 512 {
		return 0, ErrInvalidArgument
	}
	release, err := t.acquire(ctx)
	if err != nil {
		return 0, err
	}
	defer release()
	tx, err := t.begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var days int64
	if err = tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(retention_days),0) FROM "+t.s.tables.definitions).Scan(&days); err != nil {
		return 0, err
	}
	cutoff := now.Add(-max(time.Duration(days)*24*time.Hour, t.cfg.MaxLateness+5*time.Minute)).UnixNano()
	var id string
	err = tx.QueryRowContext(ctx, t.bind("SELECT a.id FROM "+t.table("segments")+" a WHERE a.state IN ('expired','legacy') AND a.created<? AND NOT EXISTS (SELECT 1 FROM "+t.table("summary")+" b WHERE b.segment_id=a.id) AND NOT EXISTS (SELECT 1 FROM "+t.table("receipts")+" r WHERE r.segment_id=a.id) AND NOT EXISTS (SELECT 1 FROM "+t.table("hot")+" h WHERE h.segment_id=a.id) AND NOT EXISTS (SELECT 1 FROM "+t.table("leases")+" l WHERE l.segment_id=a.id AND l.expires>?) ORDER BY a.created,a.id LIMIT 1"), cutoff, now.UnixNano()).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	rows, err := tx.QueryContext(ctx, t.bind("SELECT series_id FROM "+t.table("members")+" WHERE segment_id=? ORDER BY series_id LIMIT ?"), id, limit)
	if err != nil {
		return 0, err
	}
	var series []string
	for rows.Next() {
		var sid string
		if err = rows.Scan(&sid); err != nil {
			break
		}
		series = append(series, sid)
	}
	re := rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	if re != nil {
		return 0, re
	}
	for _, sid := range series {
		if _, err = tx.ExecContext(ctx, t.bind("DELETE FROM "+t.table("gaps")+" WHERE segment_id=? AND series_id=?"), id, sid); err != nil {
			return 0, err
		}
		if _, err = tx.ExecContext(ctx, t.bind("DELETE FROM "+t.table("members")+" WHERE segment_id=? AND series_id=?"), id, sid); err != nil {
			return 0, err
		}
		// Identity dictionaries only disappear after their final hot, summary and
		// cold ownership is gone. A new acceptance recreates the canonical ID.
		if _, err = tx.ExecContext(ctx, t.bind("DELETE FROM "+t.table("series")+" WHERE id=? AND NOT EXISTS (SELECT 1 FROM "+t.table("members")+" WHERE series_id=?) AND NOT EXISTS (SELECT 1 FROM "+t.table("hot")+" WHERE series_id=?) AND NOT EXISTS (SELECT 1 FROM "+t.table("latest")+" WHERE series_id=?) AND NOT EXISTS (SELECT 1 FROM "+t.table("summary")+" WHERE series_id=?) AND NOT EXISTS (SELECT 1 FROM "+t.table("rollup")+" b JOIN "+t.table("rollup_keys")+" k ON k.id=b.series_id WHERE k.source_id=?)"), sid, sid, sid, sid, sid, sid); err != nil {
			return 0, err
		}
		if t.s.cfg.Driver == DriverSQLite {
			// Erasure identity must survive GC so a recreated series cannot
			// expose old immutable records. Unreferenced non-erased identities
			// can be reclaimed with the same bounded catalog page.
			if _, err = tx.ExecContext(ctx, "DELETE FROM "+t.table("series_keys")+" WHERE id=CAST(substr(?,2) AS INTEGER) AND '@'||id=? AND NOT EXISTS (SELECT 1 FROM "+t.table("series")+" WHERE id=?) AND NOT EXISTS (SELECT 1 FROM "+t.table("erasures")+" WHERE series_id=?)", sid, sid, sid, sid); err != nil {
				return 0, err
			}
		}
	}
	if len(series) < limit {
		if _, err = tx.ExecContext(ctx, t.bind("DELETE FROM "+t.table("leases")+" WHERE segment_id=?"), id); err != nil {
			return 0, err
		}
		if _, err = tx.ExecContext(ctx, t.bind("DELETE FROM "+t.table("segments")+" WHERE id=?"), id); err != nil {
			return 0, err
		}
	}
	var floor int64
	if err = tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(value),0) FROM "+t.table("progress")+" WHERE task='raw_history_floor'").Scan(&floor); err != nil {
		return 0, err
	}
	if cutoff > floor {
		if err = t.setProgress(ctx, tx, "raw_history_floor", cutoff); err != nil {
			return 0, err
		}
	}
	return max(1, len(series)), tx.Commit()
}
