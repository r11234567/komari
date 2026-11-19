package metric

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

// Inspect only the cells straddling either requested edge. A recent timestamp
// alone does not prove hot retention: offline imports and pressure reclamation
// can leave a complete scalar cell without its raw population. In that case
// advertise an inward window containing whole cells, never partial statistics.
func (t *Telemetry) scalarAnswerableWindow(ctx context.Context, q Query, interval time.Duration, now time.Time) (Query, error) {
	ctx = defaultChartBudget(ctx)
	tx, shared := ctx.Value(telemetrySnapshotKey{}).(*sql.Tx)
	if !shared {
		var err error
		tx, err = t.s.reader().BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
		if err != nil {
			return q, err
		}
		defer tx.Rollback()
	}
	name := q.MetricName
	if name == sqliteVirtualPingLossMetric {
		name = sqliteMergedPingLatencyMetric
	}
	filter, args, err := t.seriesFilter(ctx, q, "s")
	if err != nil {
		return q, err
	}
	through, err := t.progress(ctx, tx, "summary")
	if err != nil {
		return q, err
	}
	hotRead := 0
	for _, left := range []bool{true, false} {
		edge := q.End
		condition := "b.bucket*1000000000+b.first_offset<=? AND b.bucket*1000000000+b.last_offset>?"
		if left {
			edge = q.Start
			condition = "b.bucket*1000000000+b.first_offset<? AND b.bucket*1000000000+b.last_offset>=?"
		}
		statement := "SELECT s.id,b.series_id,b.resolution,b.bucket,b." + strings.ReplaceAll(t.scalarColumns(), ",", ",b.") + " FROM " + t.table("series") + " s JOIN " + t.table("rollup_keys") + " k ON k.source_id=s.id JOIN " + t.table("rollup_due") + " d ON d.series_id=k.id JOIN " + t.table("rollup") + " b ON b.series_id=d.series_id AND b.resolution=d.resolution WHERE s.metric=?" + filter + " AND b.bucket>?-d.resolution AND b.bucket<=? AND " + condition
		values := append([]any{name}, args...)
		values = append(values, edge.Unix(), edge.Unix(), edge.UnixNano(), edge.UnixNano())
		rows, e := tx.QueryContext(ctx, t.bind(statement), values...)
		if e != nil {
			return q, e
		}
		type boundary struct {
			source string
			cell   scalarCell
		}
		var cells []boundary
		for rows.Next() {
			var b boundary
			scan := append([]any{&b.source, &b.cell.series, &b.cell.resolution, &b.cell.bucket}, scalarScan(&b.cell.stats)...)
			if e = rows.Scan(scan...); e != nil {
				break
			}
			if e = reserveChartMemory(ctx, 0, 512); e != nil {
				break
			}
			decodeScalarStats(&b.cell.stats, b.cell.bucket)
			cells = append(cells, b)
		}
		re := rows.Err()
		rows.Close()
		if e != nil {
			releaseChartMemory(ctx, len(cells)*512)
			return q, e
		}
		if re != nil {
			releaseChartMemory(ctx, len(cells)*512)
			return q, re
		}
		for _, b := range cells {
			points, e := t.scalarHotBoundary(ctx, tx, b.source, name, b.cell, through, now, &hotRead)
			if e != nil {
				releaseChartMemory(ctx, len(cells)*512)
				return q, e
			}
			if points != nil {
				releaseChartMemory(ctx, len(points)*256)
				continue
			}
			if left {
				at := time.Unix(b.cell.bucket+b.cell.resolution, 0).UTC()
				aligned := alignTime(at, interval)
				if aligned.Before(at) {
					aligned = aligned.Add(interval)
				}
				q.Start = maxTelemetryTime(q.Start, aligned)
			} else {
				at := alignTime(time.Unix(b.cell.bucket, 0).UTC(), interval).Add(-time.Nanosecond)
				q.End = minTime(q.End, at)
			}
		}
		releaseChartMemory(ctx, len(cells)*512)
	}
	if q.End.Before(q.Start) {
		return q, ErrTelemetryCoverage
	}
	if !shared {
		err = tx.Commit()
	}
	return q, err
}
