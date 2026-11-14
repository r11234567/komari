package metric

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
)

// ExportRawBatch is an explicit privileged cold-read API. The authenticated
// application must supply WithTelemetryEntities. Normal Query/Chart never
// invoke it. Matching metrics share one decode per immutable archive.
func (t *Telemetry) ExportRawBatch(ctx context.Context, queries []Query) ([][]Point, error) {
	if err := t.CheckCapacity(ctx, true); err != nil {
		return nil, err
	}
	if len(queries) > 64 {
		return nil, ErrTelemetryBackpressure
	}
	if _, ok := ctx.Value(telemetryScopeKey{}).([]string); !ok {
		return nil, fmt.Errorf("%w: export requires explicit entity scope", ErrInvalidArgument)
	}
	out := make([][]Point, len(queries))
	if len(queries) == 0 {
		return out, nil
	}
	for _, q := range queries {
		if err := q.Validate(); err != nil {
			return nil, err
		}
	}
	start, end := queries[0].Start, queries[0].End
	for _, q := range queries {
		start = minTime(start, q.Start)
		end = maxTelemetryTime(end, q.End)
	}
	if end.Sub(start) > 2*time.Hour {
		return nil, ErrTelemetryBackpressure
	}
	lease := telemetryID()
	release, err := t.acquire(ctx)
	if err != nil {
		return nil, err
	}
	tx, err := t.begin(ctx)
	if err != nil {
		release()
		return nil, err
	}
	defer tx.Rollback()
	watermark, err := t.progress(ctx, tx, "ingest")
	if err != nil {
		release()
		return nil, err
	}
	var historyFloor int64
	if err = tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(value),0) FROM "+t.table("progress")+" WHERE task='raw_history_floor'").Scan(&historyFloor); err != nil {
		release()
		return nil, err
	}
	if historyFloor > 0 && start.UnixNano() < historyFloor {
		release()
		return nil, fmt.Errorf("%w: raw telemetry history has expired", ErrTelemetryCoverage)
	}
	// Missing old-core membership must not silently produce an empty export.
	var missing string
	err = tx.QueryRowContext(ctx, "SELECT a.id FROM "+t.table("segments")+" a WHERE a.points>0 AND a.state<>'expired' AND NOT EXISTS (SELECT 1 FROM "+t.table("members")+" m WHERE m.segment_id=a.id) LIMIT 1").Scan(&missing)
	if err == nil {
		release()
		return nil, fmt.Errorf("%w: archive membership indexing pending", ErrTelemetryCoverage)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		release()
		return nil, err
	}
	ids := make(map[string]bool)
	for _, q := range queries {
		physical := q
		if physical.MetricName == sqliteVirtualPingLossMetric {
			physical.MetricName = sqliteMergedPingLatencyMetric
		}
		filter, args, e := t.seriesFilter(ctx, physical, "s")
		if e != nil {
			release()
			return nil, e
		}
		// A summary-only import is useful for charts, but must never produce a
		// successful CSV that silently omits the unrecoverable source raw.
		gapArgs := []any{physical.MetricName, q.End.UnixNano(), q.Start.UnixNano()}
		gapArgs = append(gapArgs, args...)
		hasGap, e := t.exportHasLegacyGap(ctx, tx, "s.metric=? AND g.first_observed<=? AND g.last_observed>=? AND (e.through_id IS NULL OR a.last_id>e.through_id)"+filter, gapArgs, q.Start.UnixNano(), q.End.UnixNano())
		if e != nil {
			release()
			return nil, e
		}
		if hasGap {
			release()
			return nil, fmt.Errorf("%w: legacy raw was already removed before migration", ErrTelemetryCoverage)
		}
		if t.scalar {
			missing, err := t.scalarExportHasGapFrom(ctx, tx, physical)
			if err != nil || missing {
				release()
				if err != nil {
					return nil, err
				}
				return nil, fmt.Errorf("%w: migrated history has no original observations", ErrTelemetryCoverage)
			}
		}
		statement := "SELECT DISTINCT m.segment_id FROM " + t.table("members") + " m JOIN " + t.table("series") + " s ON s.id=m.series_id JOIN " + t.table("segments") + " a ON a.id=m.segment_id LEFT JOIN " + t.table("erasures") + " e ON e.series_id=s.id WHERE s.metric=? AND m.first_observed<=? AND m.last_observed>=? AND a.first_id<=? AND a.state<>'legacy' AND (e.through_id IS NULL OR a.last_id>e.through_id)" + filter + " LIMIT 2049"
		values := []any{physical.MetricName, q.End.UnixNano(), q.Start.UnixNano(), watermark}
		values = append(values, args...)
		rows, e := tx.QueryContext(ctx, t.bind(statement), values...)
		if e != nil {
			release()
			return nil, e
		}
		for rows.Next() {
			var id string
			if e = rows.Scan(&id); e != nil {
				break
			}
			ids[id] = true
			if len(ids) > 2048 {
				e = ErrTelemetryBackpressure
				break
			}
		}
		re := rows.Err()
		rows.Close()
		if e != nil {
			release()
			return nil, e
		}
		if re != nil {
			release()
			return nil, re
		}
	}
	var segments []telemetrySegment
	for id := range ids {
		seg, e := t.segment(ctx, tx, id)
		if e != nil {
			release()
			return nil, e
		}
		if _, e = tx.ExecContext(ctx, t.bind("INSERT INTO "+t.table("leases")+" (id,segment_id,expires) VALUES (?,?,?)"), lease, id, time.Now().Add(10*time.Minute).UnixNano()); e != nil {
			release()
			return nil, e
		}
		segments = append(segments, seg)
	}
	if err = tx.Commit(); err != nil {
		release()
		return nil, err
	}
	release()
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		t.s.db.ExecContext(cleanup, t.bind("DELETE FROM "+t.table("leases")+" WHERE id=?"), lease)
	}()
	if err = t.fault("export-pinned"); err != nil {
		return nil, err
	}
	sort.Slice(segments, func(i, j int) bool { return segments[i].First < segments[j].First })
	// Tombstones are bounded by selected series and apply to both SQL and cold
	// records, including exports started after an administrator erased history.
	erased := make(map[string]int64)
	for _, q := range queries {
		physical := q
		if physical.MetricName == sqliteVirtualPingLossMetric {
			physical.MetricName = sqliteMergedPingLatencyMetric
		}
		filter, args, e := t.seriesFilter(ctx, physical, "s")
		if e != nil {
			return nil, e
		}
		rows, e := t.s.reader().QueryContext(ctx, t.bind("SELECT "+t.canonicalSeriesSQL("s")+",e.through_id FROM "+t.table("erasures")+" e JOIN "+t.table("series")+" s ON s.id=e.series_id WHERE s.metric=?"+filter+" LIMIT 16385"), append([]any{physical.MetricName}, args...)...)
		if e != nil {
			return nil, e
		}
		for rows.Next() {
			var id string
			var through int64
			if e = rows.Scan(&id, &through); e != nil {
				break
			}
			erased[id] = through
			if len(erased) > 16384 {
				e = ErrTelemetryBackpressure
				break
			}
		}
		re := rows.Err()
		rows.Close()
		if e != nil {
			return nil, e
		}
		if re != nil {
			return nil, re
		}
	}
	budget := 0
	count := 0
	lastRenew := time.Now()
	visit := func(r telemetryRecord) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if time.Since(lastRenew) > time.Minute {
			if _, e := t.s.db.ExecContext(ctx, t.bind("UPDATE "+t.table("leases")+" SET expires=? WHERE id=?"), time.Now().Add(10*time.Minute).UnixNano(), lease); e != nil {
				return e
			}
			lastRenew = time.Now()
		}
		if r.Point == nil || r.Ingest > watermark {
			return nil
		}
		p := r.Point.normalized()
		tags, _ := json.Marshal(p.Tags)
		h := sha256.Sum256([]byte(p.MetricName + "\x00" + p.EntityID + "\x00" + string(tags)))
		if r.Ingest <= erased[hex.EncodeToString(h[:])] {
			return nil
		}
		for i, q := range queries {
			candidate := p
			if q.MetricName == sqliteVirtualPingLossMetric && p.MetricName == sqliteMergedPingLatencyMetric {
				candidate = virtualPingLossPoint(p)
			}
			if !telemetryPointMatches(ctx, candidate, q) {
				continue
			}
			encoded, _ := json.Marshal(candidate)
			budget += len(encoded) + 128
			count++
			if budget > 24<<20 || count > 100000 {
				return ErrTelemetryBackpressure
			}
			out[i] = append(out[i], candidate)
		}
		return nil
	}
	for _, seg := range segments {
		switch seg.State {
		case "archived", "deleting", "complete":
			err = t.visitArchive(ctx, seg, visit)
		case "open", "sealed", "writing":
			var ingest int64
			ordinal := -1
			for {
				page, e := t.segmentPage(ctx, seg.ID, ingest, ordinal, 128)
				if e != nil {
					err = e
					break
				}
				if len(page) == 0 {
					break
				}
				for _, r := range page {
					if err = visit(r); err != nil {
						break
					}
					ingest, ordinal = r.Ingest, r.Ordinal
				}
				if err != nil {
					break
				}
			}
		default:
			err = ErrTelemetryCoverage
		}
		if err != nil {
			return nil, err
		}
	}
	for i, q := range queries {
		sort.SliceStable(out[i], func(a, b int) bool {
			if q.Order == OrderDesc {
				return out[i][a].Timestamp.After(out[i][b].Timestamp)
			}
			return out[i][a].Timestamp.Before(out[i][b].Timestamp)
		})
		from := min(q.Offset, len(out[i]))
		to := len(out[i])
		if q.Limit > 0 {
			to = min(to, from+q.Limit)
		}
		out[i] = out[i][from:to]
	}
	return out, nil
}

func telemetryPointMatches(ctx context.Context, p Point, q Query) bool {
	if p.MetricName != q.MetricName || q.EntityID != "" && p.EntityID != q.EntityID || p.Timestamp.Before(q.Start) || p.Timestamp.After(q.End) {
		return false
	}
	if ids, ok := ctx.Value(telemetryScopeKey{}).([]string); ok {
		found := false
		for _, id := range ids {
			if id == p.EntityID {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	for k, v := range q.Tags {
		if p.Tags[k] != v {
			return false
		}
	}
	return true
}
func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
func maxTelemetryTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// IndexLegacyCoreSegment builds the bounded membership index for a sealed
// v1/v2 experimental segment. It is explicit work, never an Open-time scan.
func (t *Telemetry) IndexLegacyCoreSegment(ctx context.Context, id string) error {
	seg, err := t.segment(ctx, t.s.reader(), id)
	if err != nil {
		return err
	}
	if seg.State == "open" || seg.State == "writing" {
		return ErrTelemetryBackpressure
	}
	type member struct{ first, last, n int64 }
	members := map[string]member{}
	visit := func(r telemetryRecord) error {
		p := r.Point
		tags, _ := json.Marshal(p.Tags)
		h := sha256.Sum256([]byte(p.MetricName + "\x00" + p.EntityID + "\x00" + string(tags)))
		id := hex.EncodeToString(h[:])
		m := members[id]
		ns := p.Timestamp.UnixNano()
		if m.n == 0 {
			m.first = ns
		}
		m.first = min(m.first, ns)
		m.last = max(m.last, ns)
		m.n++
		members[id] = m
		if len(members) > 16384 {
			return ErrTelemetryBackpressure
		}
		return nil
	}
	if seg.State == "sealed" {
		var i int64
		o := -1
		for {
			page, e := t.segmentPage(ctx, id, i, o, 128)
			if e != nil {
				return e
			}
			if len(page) == 0 {
				break
			}
			for _, r := range page {
				if e = visit(r); e != nil {
					return e
				}
				i, o = r.Ingest, r.Ordinal
			}
		}
	} else if err = t.visitArchive(ctx, seg, visit); err != nil {
		return err
	}
	release, err := t.acquire(ctx)
	if err != nil {
		return err
	}
	defer release()
	tx, err := t.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for sid, m := range members {
		sid, err = t.seriesKey(ctx, tx, sid)
		if err != nil {
			return err
		}
		q := "INSERT INTO " + t.table("members") + " (segment_id,series_id,first_observed,last_observed,points) VALUES (?,?,?,?,?)"
		if t.s.cfg.Driver == DriverMySQL {
			q += " ON DUPLICATE KEY UPDATE points=VALUES(points),first_observed=VALUES(first_observed),last_observed=VALUES(last_observed)"
		} else {
			q += " ON CONFLICT(segment_id,series_id) DO UPDATE SET points=excluded.points,first_observed=excluded.first_observed,last_observed=excluded.last_observed"
		}
		if _, err = tx.ExecContext(ctx, t.bind(q), id, sid, m.first, m.last, m.n); err != nil {
			return err
		}
	}
	return tx.Commit()
}
