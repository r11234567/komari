package metric

import (
	"context"
	"encoding/json"
	"sort"
)

// EraseSeries hides old history in hot queries, summaries, cold exports and
// future backups carrying these tombstones. It does not claim to scrub bytes
// out of a shared immutable file: physical collection waits until every member
// expires. Independent traffic accounting is deliberately retained.
// The application's deletion guard must block writes for a deleted node/task.
func (t *Telemetry) EraseSeries(ctx context.Context, q Query) (int64, error) {
	release, err := t.acquire(ctx)
	if err != nil {
		return 0, err
	}
	defer release()
	filter, args, err := t.seriesFilter(ctx, q, "s")
	if err != nil {
		return 0, err
	}
	if q.MetricName != "" {
		filter += " AND s.metric=?"
		name := q.MetricName
		if name == sqliteVirtualPingLossMetric {
			name = sqliteMergedPingLatencyMetric
		}
		args = append(args, name)
	}
	if filter == "" {
		return 0, ErrInvalidArgument
	}
	// Seal the frontier before installing tombstones so future samples never
	// share a summary cell/segment with erased history.
	tx, err := t.begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	through, err := t.progress(ctx, tx, "ingest")
	if err != nil {
		return 0, err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE "+t.table("segments")+" SET state='sealed' WHERE state='open'"); err != nil {
		return 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	var total int64
	after := ""
	for {
		rows, e := t.s.reader().QueryContext(ctx, t.bind("SELECT s.id FROM "+t.table("series")+" s WHERE s.id>?"+filter+" ORDER BY s.id LIMIT 128"), append([]any{after}, args...)...)
		if e != nil {
			return total, e
		}
		var ids []string
		for rows.Next() {
			var id string
			if e = rows.Scan(&id); e != nil {
				break
			}
			ids = append(ids, id)
		}
		re := rows.Err()
		rows.Close()
		if e != nil {
			return total, e
		}
		if re != nil {
			return total, re
		}
		if len(ids) == 0 {
			return total, nil
		}
		tx, e = t.begin(ctx)
		if e != nil {
			return total, e
		}
		for _, id := range ids {
			statement := "INSERT INTO " + t.table("erasures") + " (series_id,through_id) VALUES (?,?)"
			if t.s.cfg.Driver == DriverMySQL {
				statement += " ON DUPLICATE KEY UPDATE through_id=GREATEST(through_id,VALUES(through_id))"
			} else {
				statement += " ON CONFLICT(series_id) DO UPDATE SET through_id=CASE WHEN excluded.through_id>" + t.table("erasures") + ".through_id THEN excluded.through_id ELSE " + t.table("erasures") + ".through_id END"
			}
			if _, e = tx.ExecContext(ctx, t.bind(statement), id, through); e != nil {
				tx.Rollback()
				return total, e
			}
		}
		if e = tx.Commit(); e != nil {
			return total, e
		}
		total += int64(len(ids))
		after = ids[len(ids)-1]
	}
}

// PruneErasedChunk physically removes bounded summary cells already hidden by
// erasure. Raw rows remain owned by archive/deletion barriers until handoff.
func (t *Telemetry) PruneErasedChunk(ctx context.Context, limit int) (int, error) {
	if limit <= 0 || limit > 512 {
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
	rows, err := tx.QueryContext(ctx, t.bind("SELECT b.segment_id,b.series_id,b.resolution,b.bucket FROM "+t.table("summary")+" b JOIN "+t.table("segments")+" a ON a.id=b.segment_id JOIN "+t.table("erasures")+" e ON e.series_id=b.series_id WHERE a.last_id<=e.through_id ORDER BY b.series_id,b.segment_id,b.resolution,b.bucket LIMIT ?"), limit)
	if err != nil {
		return 0, err
	}
	var keys []telemetrySummaryKey
	for rows.Next() {
		var k telemetrySummaryKey
		if err = rows.Scan(&k.segment, &k.series, &k.resolution, &k.bucket); err != nil {
			break
		}
		keys = append(keys, k)
	}
	re := rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	if re != nil {
		return 0, re
	}
	for _, k := range keys {
		if _, err = tx.ExecContext(ctx, t.bind("DELETE FROM "+t.table("summary")+" WHERE segment_id=? AND series_id=? AND resolution=? AND bucket=?"), k.segment, k.series, k.resolution, k.bucket); err != nil {
			return 0, err
		}
	}
	return len(keys), tx.Commit()
}

// EntityIDs reads the small series dictionary instead of raw history. Exact
// time membership is checked against segment bounds; retained tombstones alone
// never restore a deleted node in the administrative node list.
func (t *Telemetry) EntityIDs(ctx context.Context, q Query) ([]string, error) {
	filter, args, err := t.seriesFilter(ctx, q, "s")
	if err != nil {
		return nil, err
	}
	if q.MetricName != "" {
		name := q.MetricName
		if name == sqliteVirtualPingLossMetric {
			name = sqliteMergedPingLatencyMetric
		}
		filter += " AND s.metric=?"
		args = append(args, name)
	}
	statement := "SELECT DISTINCT s.entity FROM " + t.table("series") + " s JOIN " + t.table("members") + " m ON m.series_id=s.id JOIN " + t.table("segments") + " a ON a.id=m.segment_id LEFT JOIN " + t.table("erasures") + " e ON e.series_id=s.id WHERE (e.through_id IS NULL OR a.last_id>e.through_id)" + filter
	if !q.Start.IsZero() {
		statement += " AND m.last_observed>=?"
		args = append(args, q.Start.UnixNano())
	}
	if !q.End.IsZero() {
		statement += " AND m.first_observed<=?"
		args = append(args, q.End.UnixNano())
	}
	rows, err := t.s.reader().QueryContext(ctx, t.bind(statement+" ORDER BY s.entity LIMIT 10001"), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
		if len(ids) > 10000 {
			return nil, ErrTelemetryBackpressure
		}
	}
	return ids, rows.Err()
}

func (t *Telemetry) MetricTagValues(ctx context.Context, name, tag string) ([]string, error) {
	if name == sqliteVirtualPingLossMetric {
		name = sqliteMergedPingLatencyMetric
	}
	rows, err := t.s.reader().QueryContext(ctx, t.bind("SELECT DISTINCT s.tags FROM "+t.table("series")+" s JOIN "+t.table("members")+" m ON m.series_id=s.id JOIN "+t.table("segments")+" a ON a.id=m.segment_id LEFT JOIN "+t.table("erasures")+" e ON e.series_id=s.id WHERE s.metric=? AND (e.through_id IS NULL OR a.last_id>e.through_id) LIMIT 16385"), name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := map[string]bool{}
	n := 0
	for rows.Next() {
		n++
		if n > 16384 {
			return nil, ErrTelemetryBackpressure
		}
		var raw string
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		var tags map[string]string
		if err = json.Unmarshal([]byte(raw), &tags); err != nil {
			return nil, err
		}
		if v, ok := tags[tag]; ok {
			values[v] = true
		}
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(values))
	for v := range values {
		out = append(out, v)
	}
	sort.Strings(out)
	return out, nil
}
