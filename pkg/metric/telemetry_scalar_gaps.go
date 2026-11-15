package metric

import (
	"context"
	"database/sql"
)

// A migrated chart bucket may outlive the original observations. Record only
// that absence, coalesced by series. It needs no segment owner, hash or blob.
func (t *Telemetry) putScalarRawGap(ctx context.Context, tx *sql.Tx, series, first, last int64) error {
	rows, err := tx.QueryContext(ctx, t.bind("SELECT first_ns,last_ns FROM "+t.table("raw_gaps")+" WHERE series_id=? AND first_ns<=? AND last_ns>=? ORDER BY first_ns"), series, last+1, first-1)
	if err != nil {
		return err
	}
	var remove []int64
	for rows.Next() {
		var a, b int64
		if err = rows.Scan(&a, &b); err != nil {
			break
		}
		remove = append(remove, a)
		first = min(first, a)
		last = max(last, b)
	}
	re := rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if re != nil {
		return re
	}
	for _, a := range remove {
		if _, err = tx.ExecContext(ctx, t.bind("DELETE FROM "+t.table("raw_gaps")+" WHERE series_id=? AND first_ns=?"), series, a); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, t.bind("INSERT INTO "+t.table("raw_gaps")+" (series_id,first_ns,last_ns) VALUES (?,?,?)"), series, first, last)
	return err
}

func (t *Telemetry) scalarExportHasGap(ctx context.Context, q Query) (bool, error) {
	return t.scalarExportHasGapFrom(ctx, t.s.reader(), q)
}

func (t *Telemetry) scalarExportHasGapFrom(ctx context.Context, db querier, q Query) (bool, error) {
	filter, args, err := t.seriesFilter(ctx, q, "s")
	if err != nil {
		return false, err
	}
	params := append([]any{q.MetricName, q.End.UnixNano(), q.Start.UnixNano()}, args...)
	var n int
	err = db.QueryRowContext(ctx, t.bind("SELECT COUNT(*) FROM "+t.table("raw_gaps")+" g JOIN "+t.table("rollup_keys")+" k ON k.id=g.series_id JOIN "+t.table("series")+" s ON s.id=k.source_id WHERE s.metric=? AND g.first_ns<=? AND g.last_ns>=?"+filter), params...).Scan(&n)
	return n > 0, err
}
