package metric

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// PruneCatalogChunk retires expired raw ownership independently of chart
// retention. The durable raw floor survives removal of the catalog rows.
// Legacy gap owners still retain their conservative compatibility horizon.
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
	merged := 0
	for attempts := 0; attempts < 32 && merged < limit; attempts++ {
		n, e := t.mergeCatalogPair(ctx, tx, now, limit-merged)
		if e != nil {
			return 0, e
		}
		if n == 0 {
			break
		}
		merged += n
	}
	if merged > 0 {
		return merged, tx.Commit()
	}
	var days int64
	if err = tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(retention_days),0) FROM "+t.s.tables.definitions).Scan(&days); err != nil {
		return 0, err
	}
	cutoff := now.Add(-max(time.Duration(days)*24*time.Hour, t.cfg.MaxLateness+5*time.Minute)).UnixNano()
	var id string
	err = tx.QueryRowContext(ctx, t.bind("SELECT a.id FROM "+t.table("segments")+" a WHERE (a.state='expired' OR (a.state='legacy' AND a.created<?)) AND NOT EXISTS (SELECT 1 FROM "+t.table("summary")+" b WHERE b.segment_id=a.id) AND NOT EXISTS (SELECT 1 FROM "+t.table("receipts")+" r WHERE r.segment_id=a.id) AND NOT EXISTS (SELECT 1 FROM "+t.table("hot")+" h WHERE h.segment_id=a.id) AND NOT EXISTS (SELECT 1 FROM "+t.table("leases")+" l WHERE l.segment_id=a.id AND l.expires>?) ORDER BY a.created,a.id LIMIT 1"), cutoff, now.UnixNano()).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		// Raw owners may disappear before their chart population. Reclaim
		// dictionaries independently after that final population retires.
		n, e := t.pruneUnusedTelemetrySeries(ctx, tx, limit)
		if e != nil {
			return 0, e
		}
		return n, tx.Commit()
	}
	if err != nil {
		return 0, err
	}
	// Upgraded databases may already contain expired owners from before the
	// expiry transaction published a floor. Preserve their missing-history
	// evidence before deleting any member, including chunked catalog removal.
	var last, floor int64
	if err = tx.QueryRowContext(ctx, t.bind("SELECT COALESCE(MAX(last_observed),0) FROM "+t.table("members")+" WHERE segment_id=?"), id).Scan(&last); err != nil {
		return 0, err
	}
	if err = tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(value),0) FROM "+t.table("progress")+" WHERE task='raw_history_floor'").Scan(&floor); err != nil {
		return 0, err
	}
	if last >= floor && last > 0 {
		if err = t.setProgress(ctx, tx, "raw_history_floor", last+1); err != nil {
			return 0, err
		}
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
	}
	if len(series) < limit {
		if _, err = tx.ExecContext(ctx, t.bind("DELETE FROM "+t.table("leases")+" WHERE segment_id=?"), id); err != nil {
			return 0, err
		}
		if _, err = tx.ExecContext(ctx, t.bind("DELETE FROM "+t.table("segments")+" WHERE id=?"), id); err != nil {
			return 0, err
		}
	}
	return max(1, len(series)), tx.Commit()
}

func (t *Telemetry) pruneUnusedTelemetrySeries(ctx context.Context, tx *sql.Tx, limit int) (int, error) {
	rows, err := tx.QueryContext(ctx, t.bind("SELECT s.id FROM "+t.table("series")+" s WHERE NOT EXISTS (SELECT 1 FROM "+t.table("members")+" m WHERE m.series_id=s.id) AND NOT EXISTS (SELECT 1 FROM "+t.table("hot")+" h WHERE h.series_id=s.id) AND NOT EXISTS (SELECT 1 FROM "+t.table("latest")+" l WHERE l.series_id=s.id) AND NOT EXISTS (SELECT 1 FROM "+t.table("summary")+" b WHERE b.series_id=s.id) AND NOT EXISTS (SELECT 1 FROM "+t.table("rollup")+" b JOIN "+t.table("rollup_keys")+" k ON k.id=b.series_id WHERE k.source_id=s.id) AND NOT EXISTS (SELECT 1 FROM "+t.table("raw_gaps")+" g JOIN "+t.table("rollup_keys")+" k ON k.id=g.series_id WHERE k.source_id=s.id) ORDER BY s.id LIMIT ?"), limit)
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			break
		}
		ids = append(ids, id)
	}
	re := rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	if re != nil {
		return 0, re
	}
	for _, id := range ids {
		if _, err = tx.ExecContext(ctx, t.bind("DELETE FROM "+t.table("rollup_due")+" WHERE series_id IN (SELECT id FROM "+t.table("rollup_keys")+" WHERE source_id=?)"), id); err != nil {
			return 0, err
		}
		if _, err = tx.ExecContext(ctx, t.bind("DELETE FROM "+t.table("rollup_keys")+" WHERE source_id=?"), id); err != nil {
			return 0, err
		}
		if _, err = tx.ExecContext(ctx, t.bind("DELETE FROM "+t.table("series")+" WHERE id=?"), id); err != nil {
			return 0, err
		}
		if t.s.cfg.Driver == DriverSQLite {
			// Erased identities must survive so recreation cannot expose an
			// old immutable record under a reused short dictionary key.
			if _, err = tx.ExecContext(ctx, "DELETE FROM "+t.table("series_keys")+" WHERE id=CAST(substr(?,2) AS INTEGER) AND '@'||id=? AND NOT EXISTS (SELECT 1 FROM "+t.table("erasures")+" WHERE series_id=?)", id, id, id); err != nil {
				return 0, err
			}
		}
	}
	return len(ids), nil
}
