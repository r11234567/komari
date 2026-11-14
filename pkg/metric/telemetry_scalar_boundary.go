package metric

import (
	"context"
	"database/sql"
	"time"
)

// A retained scalar cell cannot be split. Its recent raw population can: use
// it only while the complete cell is in the live window and all summarized
// observations are still present in this same read snapshot. This is a bounded
// hot-table lookup, never a cold-file read or a new persisted representation.
func (t *Telemetry) scalarHotBoundary(ctx context.Context, tx *sql.Tx, source, name string, cell scalarCell, through int64, now time.Time, hotRead *int) ([]telemetryStats, error) {
	if cell.stats.FirstNano < now.Add(-t.cfg.RawWindow).UnixNano() {
		return nil, nil
	}
	// Additive coarse/fine cells may coexist after late input or migration.
	// Their raw populations cannot be attributed to this one cell by time alone.
	var other int
	err := tx.QueryRowContext(ctx, t.bind("SELECT 1 FROM "+t.table("rollup_due")+" d JOIN "+t.table("rollup")+" b ON b.series_id=d.series_id AND b.resolution=d.resolution WHERE b.series_id=? AND b.resolution<>? AND b.bucket<? AND b.bucket+b.resolution>? LIMIT 1"), cell.series, cell.resolution, cell.bucket+cell.resolution, cell.bucket).Scan(&other)
	if err == nil {
		return nil, nil
	}
	if err != sql.ErrNoRows {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, t.bind("SELECT h.observed,h.value,h.ingest_id,h.ordinal FROM "+t.table("hot")+" h LEFT JOIN "+t.table("erasures")+" e ON e.series_id=h.series_id WHERE h.series_id=? AND h.ingest_id<=? AND h.observed>=? AND h.observed<? AND (e.through_id IS NULL OR h.ingest_id>e.through_id) ORDER BY h.ingest_id,h.ordinal LIMIT ?"), source, through, cell.bucket*int64(time.Second), (cell.bucket+cell.resolution)*int64(time.Second), 10001-*hotRead)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var points []telemetryStats
	keep := false
	defer func() {
		if !keep {
			releaseChartMemory(ctx, 256*len(points))
		}
	}()
	for rows.Next() {
		*hotRead += 1
		if *hotRead > 10000 {
			return nil, ErrTelemetryBackpressure
		}
		var at, ingest int64
		var ordinal int
		var value float64
		if err = rows.Scan(&at, &value, &ingest, &ordinal); err != nil {
			return nil, err
		}
		if err = reserveChartMemory(ctx, 0, 256); err != nil {
			return nil, err
		}
		var st telemetryStats
		addScalarStats(&st, Point{MetricName: name, Timestamp: time.Unix(0, at), Value: value}, ingest, ordinal)
		points = append(points, st)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	// Raw is a subset of this sole canonical cell. Equality of the existing
	// population count proves no observation was retired; never fill gaps with
	// a smaller subset, even if all remaining points fit the requested window.
	if int64(len(points)) != cell.stats.Attempts {
		return nil, nil
	}
	keep = true
	return points, nil
}
