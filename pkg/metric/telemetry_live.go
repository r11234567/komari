package metric

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"time"
)

// Live observations and archive handoff have different lifetimes. A handed-off
// observation keeps its existing hot row, with no segment owner, until the
// display window expires. CSV-only deltas never enter this display retention.
func telemetryLiveMetric(name string) bool {
	return name != "traffic.up" && name != "traffic.down"
}

func telemetryLiveBytes(_ string, labels string) int64 { return int64(256 + len(labels)) }

func (t *Telemetry) pruneLatest(ctx context.Context, name string, cutoff time.Time, limit int) (int, error) {
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
	rows, err := tx.QueryContext(ctx, t.bind("SELECT l.series_id FROM "+t.table("latest")+" l JOIN "+t.table("series")+" s ON s.id=l.series_id WHERE s.metric=? AND l.observed<? ORDER BY l.series_id LIMIT ?"), name, cutoff.UnixNano(), limit)
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
		if _, err = tx.ExecContext(ctx, t.bind("DELETE FROM "+t.table("latest")+" WHERE series_id=? AND observed<?"), id, cutoff.UnixNano()); err != nil {
			return 0, err
		}
	}
	return len(ids), tx.Commit()
}

func (t *Telemetry) createLatestSchema(ctx context.Context) error {
	suffix := ""
	if t.s.cfg.Driver == DriverSQLite {
		suffix = " WITHOUT ROWID"
	}
	_, err := t.s.db.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS "+t.table("latest")+" (series_id VARCHAR(64) NOT NULL PRIMARY KEY,observed BIGINT NOT NULL,ingest_id BIGINT NOT NULL,ordinal INTEGER NOT NULL,value DOUBLE PRECISION NOT NULL,labels TEXT NOT NULL)"+suffix)
	return err
}

// The caller has already selected the newest observation for this batch and
// series. Old/late batches cannot replace a newer persisted last observation.
func (t *Telemetry) putLatest(ctx context.Context, tx *sql.Tx, sid string, ingest int64, ordinal int, p Point) error {
	if !telemetryLiveMetric(p.MetricName) {
		return nil
	}
	var at, prior int64
	var ord int
	err := tx.QueryRowContext(ctx, t.bind("SELECT observed,ingest_id,ordinal FROM "+t.table("latest")+" WHERE series_id=?"), sid).Scan(&at, &prior, &ord)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil && (at > p.Timestamp.UnixNano() || at == p.Timestamp.UnixNano() && (prior > ingest || prior == ingest && ord >= ordinal)) {
		return nil
	}
	labels, err := json.Marshal(p.Labels)
	if err != nil {
		return err
	}
	q := "INSERT INTO " + t.table("latest") + " (series_id,observed,ingest_id,ordinal,value,labels) VALUES (?,?,?,?,?,?)"
	if t.s.cfg.Driver == DriverMySQL {
		q += " ON DUPLICATE KEY UPDATE observed=VALUES(observed),ingest_id=VALUES(ingest_id),ordinal=VALUES(ordinal),value=VALUES(value),labels=VALUES(labels)"
	} else {
		q += " ON CONFLICT(series_id) DO UPDATE SET observed=excluded.observed,ingest_id=excluded.ingest_id,ordinal=excluded.ordinal,value=excluded.value,labels=excluded.labels"
	}
	_, err = tx.ExecContext(ctx, t.bind(q), sid, p.Timestamp.UnixNano(), ingest, ordinal, p.Value, string(labels))
	return err
}

func (t *Telemetry) pruneLive(ctx context.Context, tx *sql.Tx, now time.Time, limit int) (int, error) {
	rows, err := tx.QueryContext(ctx, t.bind("SELECT h.ingest_id,h.ordinal,h.series_id,h.labels FROM "+t.table("hot")+" h LEFT JOIN "+t.table("erasures")+" e ON e.series_id=h.series_id WHERE h.segment_id='' AND (h.observed<? OR h.ingest_id<=e.through_id) ORDER BY h.observed,h.ingest_id,h.ordinal LIMIT ?"), now.Add(-t.cfg.RawWindow).UnixNano(), limit)
	if err != nil {
		return 0, err
	}
	type row struct {
		ingest         int64
		ordinal        int
		series, labels string
	}
	var keys []row
	for rows.Next() {
		var k row
		if err = rows.Scan(&k.ingest, &k.ordinal, &k.series, &k.labels); err != nil {
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
	var freed int64
	for _, k := range keys {
		if _, err = tx.ExecContext(ctx, t.bind("DELETE FROM "+t.table("hot")+" WHERE ingest_id=? AND ordinal=? AND segment_id=''"), k.ingest, k.ordinal); err != nil {
			return 0, err
		}
		freed += telemetryLiveBytes(k.series, k.labels)
	}
	if freed > 0 {
		n, e := t.progress(ctx, tx, "live_bytes")
		if e != nil {
			return 0, e
		}
		if e = t.advance(ctx, tx, "live_bytes", n, max(0, n-freed)); e != nil {
			return 0, e
		}
	}
	return len(keys), nil
}

// latestRetained never reads cold files. Last snapshots survive an idle/offline
// node; scalar last observations provide an upgrade fallback. A historical
// boundary crossing an indivisible bucket is explicitly unavailable unless
// the complete boundary interval is still in the live/raw window.
func (t *Telemetry) latestRetained(ctx context.Context, name, entity string, before time.Time, limit int) ([]Point, error) {
	if limit < 1 || limit > 10000 {
		return nil, ErrInvalidArgument
	}
	physical := name
	if physical == sqliteVirtualPingLossMetric {
		physical = sqliteMergedPingLatencyMetric
	}
	tx, err := t.s.reader().BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, t.bind("SELECT s.id,s.tags,COALESCE(e.through_id,0) FROM "+t.table("series")+" s LEFT JOIN "+t.table("erasures")+" e ON e.series_id=s.id WHERE s.metric=? AND s.entity=? ORDER BY s.id LIMIT 1025"), physical, entity)
	if err != nil {
		return nil, err
	}
	type series struct {
		id, tags string
		erased   int64
	}
	var list []series
	for rows.Next() {
		var s series
		if err = rows.Scan(&s.id, &s.tags, &s.erased); err != nil {
			break
		}
		list = append(list, s)
	}
	re := rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if re != nil {
		return nil, re
	}
	if len(list) > 1024 {
		return nil, ErrTelemetryBackpressure
	}
	var out []Point
	var ambiguous int64
	pastLatest := false
	appendPoint := func(s series, ns int64, value float64, labels string) error {
		p := Point{MetricName: physical, EntityID: entity, Timestamp: time.Unix(0, ns).UTC(), Value: value}
		if e := json.Unmarshal([]byte(s.tags), &p.Tags); e != nil {
			return e
		}
		if labels != "" {
			if e := json.Unmarshal([]byte(labels), &p.Labels); e != nil {
				return e
			}
		}
		if name == sqliteVirtualPingLossMetric {
			p = virtualPingLossPoint(p)
		}
		out = append(out, p)
		return nil
	}
	for _, s := range list {
		for _, table := range []string{"latest", "hot"} {
			q := "SELECT observed,value,labels FROM " + t.table(table) + " WHERE series_id=? AND ingest_id>? AND observed<? ORDER BY observed DESC,ingest_id DESC,ordinal DESC LIMIT ?"
			args := []any{s.id, s.erased, before.UnixNano(), limit}
			if table == "latest" {
				q = "SELECT observed,value,labels FROM " + t.table(table) + " WHERE series_id=? AND ingest_id>?"
				args = []any{s.id, s.erased}
			}
			r, e := tx.QueryContext(ctx, t.bind(q), args...)
			if e != nil {
				return nil, e
			}
			for r.Next() {
				var ns int64
				var v float64
				var labels string
				if e = r.Scan(&ns, &v, &labels); e != nil {
					break
				}
				if ns >= before.UnixNano() {
					pastLatest = true
					if !telemetryChartMetric(physical) {
						ambiguous = max(ambiguous, before.UnixNano()-1)
					}
					continue
				}
				if e = appendPoint(s, ns, v, labels); e != nil {
					break
				}
			}
			re := r.Err()
			r.Close()
			if e != nil {
				return nil, e
			}
			if re != nil {
				return nil, re
			}
		}
		if !t.scalar || !telemetryChartMetric(physical) {
			continue
		}
		var key int64
		e := tx.QueryRowContext(ctx, t.bind("SELECT id FROM "+t.table("rollup_keys")+" WHERE source_id=?"), s.id).Scan(&key)
		if errors.Is(e, sql.ErrNoRows) {
			continue
		}
		if e != nil {
			return nil, e
		}
		r, e := tx.QueryContext(ctx, t.bind("SELECT resolution FROM "+t.table("rollup_due")+" WHERE series_id=?"), key)
		if e != nil {
			return nil, e
		}
		var resolutions []int64
		for r.Next() {
			var resolution int64
			if e = r.Scan(&resolution); e != nil {
				break
			}
			resolutions = append(resolutions, resolution)
		}
		re := r.Err()
		r.Close()
		if e != nil {
			return nil, e
		}
		if re != nil {
			return nil, re
		}
		for _, resolution := range resolutions {
			r, e := tx.QueryContext(ctx, t.bind("SELECT bucket,first_offset,last_offset,"+quoteMaintenanceIdentifier(t.s.cfg.Driver, "last_value")+" FROM "+t.table("rollup")+" WHERE series_id=? AND resolution=? AND bucket<=? ORDER BY bucket DESC LIMIT ?"), key, resolution, before.Unix(), limit+1)
			if e != nil {
				return nil, e
			}
			for r.Next() {
				var bucket, first, last int64
				var value float64
				if e = r.Scan(&bucket, &first, &last, &value); e != nil {
					break
				}
				base := bucket * int64(time.Second)
				first += base
				last += base
				if last >= before.UnixNano() {
					if first < before.UnixNano() {
						// The bucket can contain an unknown observation anywhere
						// before this boundary. Its first point is not a safe last.
						ambiguous = max(ambiguous, before.UnixNano()-1)
					}
					continue
				}
				if e = appendPoint(s, last, value, ""); e != nil {
					break
				}
			}
			re := r.Err()
			r.Close()
			if e != nil {
				return nil, e
			}
			if re != nil {
				return nil, re
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Timestamp.After(out[j].Timestamp) })
	unique := out[:0]
	seen := map[string]bool{}
	for _, p := range out {
		tags, _ := json.Marshal(p.Tags)
		key := p.Timestamp.Format(time.RFC3339Nano) + string(tags)
		if !seen[key] {
			seen[key] = true
			unique = append(unique, p)
		}
		if len(unique) == limit {
			break
		}
	}
	if ambiguous > 0 && before.Before(time.Now().UTC().Add(-t.cfg.RawWindow)) && (len(unique) == 0 || unique[0].Timestamp.UnixNano() < ambiguous) {
		return nil, ErrTelemetryCoverage
	}
	if len(unique) == 0 && pastLatest && before.Before(time.Now().UTC().Add(-t.cfg.RawWindow)) {
		return nil, ErrTelemetryCoverage
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return unique, nil
}
