package metric

// The online chart store contains ordinary scalar columns. Legacy statistics
// envelopes are read only while retiring their source rows; no chart or new
// write encodes or decompresses a statistics block.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

const scalarStatColumns = "n,loss,total,minimum,maximum,last_value,first_offset,last_offset"

type scalarCell struct {
	series, resolution, bucket int64 // seconds, except the observed bounds
	stats                      telemetryStats
}

func scalarScan(s *telemetryStats) []any {
	return []any{&s.N, &s.Loss, &s.Sum, &s.Min, &s.Max, &s.Last, &s.FirstNano, &s.LastNano}
}

func scalarArgs(s telemetryStats, bucket int64) []any {
	base := bucket * int64(time.Second)
	return []any{s.N, s.Loss, s.Sum, s.Min, s.Max, s.Last, s.FirstNano - base, s.LastNano - base}
}

func decodeScalarStats(s *telemetryStats, bucket int64) {
	s.Attempts = s.N + s.Loss
	s.FirstNano += bucket * int64(time.Second)
	s.LastNano += bucket * int64(time.Second)
}

func mergeScalarStats(dst *telemetryStats, src telemetryStats) {
	if src.Attempts == 0 {
		return
	}
	if dst.Attempts == 0 {
		*dst = src
		dst.Digest, dst.sketch, dst.M2, dst.First = nil, nil, 0, 0
		return
	}
	if src.FirstNano < dst.FirstNano {
		dst.FirstNano = src.FirstNano
	}
	if src.LastNano > dst.LastNano || (src.LastNano == dst.LastNano && (src.LastIngest > dst.LastIngest || (src.LastIngest == dst.LastIngest && src.LastOrdinal > dst.LastOrdinal))) {
		dst.Last, dst.LastNano, dst.LastIngest, dst.LastOrdinal = src.Last, src.LastNano, src.LastIngest, src.LastOrdinal
	}
	if src.N > 0 {
		if dst.N == 0 {
			dst.Min, dst.Max = src.Min, src.Max
		} else {
			dst.Min, dst.Max = min(dst.Min, src.Min), max(dst.Max, src.Max)
		}
	}
	dst.N += src.N
	dst.Attempts += src.Attempts
	dst.Loss += src.Loss
	dst.Sum += src.Sum
	if dst.N > 0 {
		dst.Mean = dst.Sum / float64(dst.N)
	}
}

func addScalarStats(dst *telemetryStats, p Point, ingest int64, ordinal int) {
	s := telemetryStats{Attempts: 1, FirstNano: p.Timestamp.UnixNano(), LastNano: p.Timestamp.UnixNano(), Last: p.Value, LastIngest: ingest, LastOrdinal: ordinal}
	if p.MetricName == sqliteMergedPingLatencyMetric && p.Value < 0 {
		s.Loss = 1
	} else {
		s.N, s.Sum, s.Min, s.Max = 1, p.Value, p.Value, p.Value
	}
	mergeScalarStats(dst, s)
}

func scalarAggregation(a Aggregation) bool {
	switch a {
	case AggAvg, AggSum, AggMin, AggMax, AggCount, AggLast, aggPingLossAvg, aggPingLossSum, aggPingLossMin, aggPingLossMax, aggPingLossLast:
		return true
	}
	return false
}

func (t *Telemetry) scalarKey(ctx context.Context, tx *sql.Tx, source string) (int64, error) {
	var id int64
	err := tx.QueryRowContext(ctx, t.bind("SELECT id FROM "+t.table("rollup_keys")+" WHERE source_id=?"), source).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	if err = tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(id),0)+1 FROM "+t.table("rollup_keys")).Scan(&id); err != nil {
		return 0, err
	}
	_, err = tx.ExecContext(ctx, t.bind("INSERT INTO "+t.table("rollup_keys")+" (id,source_id) VALUES (?,?)"), id, source)
	return id, err
}

func (t *Telemetry) putScalar(ctx context.Context, tx *sql.Tx, c scalarCell) error {
	if c.stats.Attempts == 0 {
		return nil
	}
	var prior telemetryStats
	err := tx.QueryRowContext(ctx, t.bind("SELECT "+scalarStatColumns+" FROM "+t.table("rollup")+" WHERE series_id=? AND resolution=? AND bucket=?"), c.series, c.resolution, c.bucket).Scan(scalarScan(&prior)...)
	if err == nil {
		decodeScalarStats(&prior, c.bucket)
		mergeScalarStats(&prior, c.stats)
		c.stats = prior
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	args := append([]any{c.series, c.resolution, c.bucket}, scalarArgs(c.stats, c.bucket)...)
	statement := "INSERT INTO " + t.table("rollup") + " (series_id,resolution,bucket," + scalarStatColumns + ") VALUES (" + strings.TrimSuffix(strings.Repeat("?,", len(args)), ",") + ")"
	var assignments []string
	for _, column := range strings.Split(scalarStatColumns, ",") {
		value := "excluded." + column
		if t.s.cfg.Driver == DriverMySQL {
			value = "VALUES(" + column + ")"
		}
		assignments = append(assignments, column+"="+value)
	}
	if t.s.cfg.Driver == DriverMySQL {
		statement += " ON DUPLICATE KEY UPDATE "
	} else {
		statement += " ON CONFLICT(series_id,resolution,bucket) DO UPDATE SET "
	}
	if _, err = tx.ExecContext(ctx, t.bind(statement+strings.Join(assignments, ",")), args...); err != nil {
		return err
	}
	statement = "INSERT INTO " + t.table("rollup_due") + " (series_id,resolution,bucket) VALUES (?,?,?)"
	if t.s.cfg.Driver == DriverMySQL {
		statement += " ON DUPLICATE KEY UPDATE bucket=LEAST(bucket,VALUES(bucket))"
	} else {
		statement += " ON CONFLICT(series_id,resolution) DO UPDATE SET bucket=CASE WHEN excluded.bucket<" + t.table("rollup_due") + ".bucket THEN excluded.bucket ELSE " + t.table("rollup_due") + ".bucket END"
	}
	_, err = tx.ExecContext(ctx, t.bind(statement), c.series, c.resolution, c.bucket)
	return err
}

func (t *Telemetry) refreshScalarDue(ctx context.Context, tx *sql.Tx, series, resolution int64) error {
	if _, err := tx.ExecContext(ctx, t.bind("DELETE FROM "+t.table("rollup_due")+" WHERE series_id=? AND resolution=?"), series, resolution); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, t.bind("INSERT INTO "+t.table("rollup_due")+" (series_id,resolution,bucket) SELECT series_id,resolution,MIN(bucket) FROM "+t.table("rollup")+" WHERE series_id=? AND resolution=? GROUP BY series_id,resolution"), series, resolution)
	return err
}

func (t *Telemetry) createScalarSchema(ctx context.Context) error {
	suffix := ""
	if t.s.cfg.Driver == DriverSQLite {
		suffix = " WITHOUT ROWID"
	}
	for _, statement := range []string{
		"CREATE TABLE IF NOT EXISTS " + t.table("rollup_keys") + " (id BIGINT PRIMARY KEY,source_id VARCHAR(64) NOT NULL UNIQUE)" + suffix,
		"CREATE TABLE IF NOT EXISTS " + t.table("rollup") + " (series_id BIGINT NOT NULL,resolution INTEGER NOT NULL,bucket BIGINT NOT NULL,n BIGINT NOT NULL,loss BIGINT NOT NULL,total DOUBLE PRECISION NOT NULL,minimum DOUBLE PRECISION NOT NULL,maximum DOUBLE PRECISION NOT NULL,last_value DOUBLE PRECISION NOT NULL,first_offset BIGINT NOT NULL,last_offset BIGINT NOT NULL,PRIMARY KEY(series_id,resolution,bucket))" + suffix,
		"CREATE TABLE IF NOT EXISTS " + t.table("rollup_due") + " (series_id BIGINT NOT NULL,resolution INTEGER NOT NULL,bucket BIGINT NOT NULL,PRIMARY KEY(series_id,resolution))" + suffix,
		"CREATE TABLE IF NOT EXISTS " + t.table("raw_gaps") + " (series_id BIGINT NOT NULL,first_ns BIGINT NOT NULL,last_ns BIGINT NOT NULL,PRIMARY KEY(series_id,first_ns))" + suffix,
	} {
		if _, err := t.s.db.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	if err := t.simplifyScalarSchema(ctx); err != nil {
		return err
	}
	if err := t.discardNonChartRollups(ctx); err != nil {
		return err
	}
	version, err := t.progress(ctx, t.s.db, "schema_version")
	if err != nil {
		return err
	}
	if version == 9 {
		t.scalar = true
		return nil
	}
	// Fence older binaries before retiring any source. A partially migrated
	// database must be resumed by this reader, never reopened by a v7 writer.
	if _, err := t.s.db.ExecContext(ctx, "UPDATE "+t.table("progress")+" SET value=-9 WHERE task='schema_version' AND value<>9"); err != nil {
		return err
	}
	// Move one old ownership group transactionally. Removing its source in the
	// same commit makes interruption resumable without per-window SHA receipts.
	for {
		moved, err := t.migrateScalarGroup(ctx)
		if err != nil {
			return err
		}
		if !moved {
			break
		}
		if err = telemetryYield(ctx); err != nil {
			return err
		}
	}
	var remaining int64
	if err := t.s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+t.table("summary")).Scan(&remaining); err != nil {
		return err
	}
	if remaining != 0 {
		return errors.New("legacy summary has no archive owner; migration retained for repair")
	}
	if _, err := t.s.db.ExecContext(ctx, "UPDATE "+t.table("progress")+" SET value=9 WHERE task='schema_version'"); err != nil {
		return err
	}
	t.scalar = true
	return nil
}

func (t *Telemetry) migrateScalarGroup(ctx context.Context) (bool, error) {
	tx, err := t.begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var segment, series, state, metricName string
	var last, erased int64
	err = tx.QueryRowContext(ctx, "SELECT b.segment_id,b.series_id,a.state,a.last_id,COALESCE(e.through_id,0),s.metric FROM "+t.table("summary")+" b JOIN "+t.table("segments")+" a ON a.id=b.segment_id JOIN "+t.table("series")+" s ON s.id=b.series_id LEFT JOIN "+t.table("erasures")+" e ON e.series_id=b.series_id ORDER BY b.segment_id,b.series_id LIMIT 1").Scan(&segment, &series, &state, &last, &erased, &metricName)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !telemetryChartMetric(metricName) {
		// The raw archive already owns these observations. Old statistics for
		// CSV-only metrics must not be reintroduced into the chart table.
		if _, err = tx.ExecContext(ctx, t.bind("DELETE FROM "+t.table("summary")+" WHERE segment_id=? AND series_id=?"), segment, series); err != nil {
			return false, err
		}
		return true, tx.Commit()
	}
	rows, err := tx.QueryContext(ctx, t.bind("SELECT resolution,bucket,stats FROM "+t.table("summary")+" WHERE segment_id=? AND series_id=? ORDER BY resolution DESC,bucket LIMIT 20001"), segment, series)
	if err != nil {
		return false, err
	}
	var cells []scalarCell
	for rows.Next() {
		var c scalarCell
		var raw string
		if err = rows.Scan(&c.resolution, &c.bucket, &raw); err != nil {
			break
		}
		if err = json.Unmarshal([]byte(raw), &c.stats); err != nil {
			break
		}
		c.stats.Digest, c.stats.sketch = nil, nil
		cells = append(cells, c)
	}
	readErr := rows.Err()
	rows.Close()
	if err != nil {
		return false, err
	}
	if readErr != nil {
		return false, readErr
	}
	if len(cells) > 20000 {
		return false, fmt.Errorf("%w: legacy owner exceeds scalar migration group budget", ErrTelemetryBackpressure)
	}
	if last > erased {
		id, e := t.scalarKey(ctx, tx, series)
		if e != nil {
			return false, e
		}
		var selected []scalarCell
		for _, c := range cells {
			covered := false
			if state != "legacy" {
				for _, previous := range selected {
					if c.bucket >= previous.bucket && c.bucket+c.resolution <= previous.bucket+previous.resolution {
						covered = true
						break
					}
					if c.bucket < previous.bucket+previous.resolution && c.bucket+c.resolution > previous.bucket {
						return false, fmt.Errorf("%w: non-nested legacy summary overlap; source retained", ErrTelemetryCoverage)
					}
				}
			}
			if covered {
				continue
			}
			selected = append(selected, c)
			resolution := legacyRetainedResolution(c.bucket, time.Duration(c.resolution), time.Now().UTC())
			c.series, c.bucket, c.resolution = id, alignTime(time.Unix(0, c.bucket), resolution).Unix(), int64(resolution/time.Second)
			if e = t.putScalar(ctx, tx, c); e != nil {
				return false, e
			}
		}
	}
	if _, err = tx.ExecContext(ctx, t.bind("DELETE FROM "+t.table("summary")+" WHERE segment_id=? AND series_id=?"), segment, series); err != nil {
		return false, err
	}
	if err = t.fault("scalar-migration-before-commit"); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func (t *Telemetry) summarizeScalar(ctx context.Context) (int, error) {
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
	before, err := t.progress(ctx, tx, "summary")
	if err != nil {
		return 0, err
	}
	var next int64
	var expected int
	err = tx.QueryRowContext(ctx, t.bind("SELECT COALESCE(MAX(ingest_id),0),COALESCE(SUM(points),0) FROM (SELECT ingest_id,points FROM "+t.table("receipts")+" WHERE ingest_id>? ORDER BY ingest_id LIMIT 32) pending"), before).Scan(&next, &expected)
	if err != nil {
		return 0, err
	}
	if next <= before {
		return 0, nil
	}
	rows, err := tx.QueryContext(ctx, t.bind("SELECT h.series_id,h.ingest_id,h.ordinal,h.observed,h.value,s.metric,COALESCE(e.through_id,0) FROM "+t.table("hot")+" h JOIN "+t.table("series")+" s ON s.id=h.series_id LEFT JOIN "+t.table("erasures")+" e ON e.series_id=s.id WHERE h.ingest_id>? AND h.ingest_id<=? ORDER BY h.ingest_id,h.ordinal"), before, next)
	if err != nil {
		return 0, err
	}
	type key struct {
		source             string
		resolution, bucket int64
	}
	groups := map[key]*telemetryStats{}
	now, n := time.Now().UTC(), 0
	for rows.Next() {
		var source, name string
		var ordinal int
		var ns, erased, ingest int64
		var value float64
		if err = rows.Scan(&source, &ingest, &ordinal, &ns, &value, &name, &erased); err != nil {
			break
		}
		n++
		if !telemetryChartMetric(name) {
			continue
		}
		if ingest <= erased {
			continue
		}
		at := time.Unix(0, ns)
		resolution := telemetryResolution(now.Sub(at))
		k := key{source, int64(resolution / time.Second), alignTime(at, resolution).Unix()}
		if groups[k] == nil {
			groups[k] = &telemetryStats{}
		}
		addScalarStats(groups[k], Point{MetricName: name, Timestamp: at, Value: value}, ingest, ordinal)
	}
	readErr := rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	if readErr != nil {
		return 0, readErr
	}
	if n != expected {
		return 0, errors.New("telemetry scalar input disappeared before handoff")
	}
	keys := map[string]int64{}
	for k, stats := range groups {
		id, ok := keys[k.source]
		if !ok {
			id, err = t.scalarKey(ctx, tx, k.source)
			if err != nil {
				return 0, err
			}
			keys[k.source] = id
		}
		if err = t.putScalar(ctx, tx, scalarCell{id, k.resolution, k.bucket, *stats}); err != nil {
			return 0, err
		}
	}
	if err = t.advance(ctx, tx, "summary", before, next); err != nil {
		return 0, err
	}
	if err = t.fault("summary-before-commit"); err != nil {
		return 0, err
	}
	return max(n, 1), tx.Commit()
}

func (t *Telemetry) promoteScalar(ctx context.Context, now time.Time, limit int) (int, error) {
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
	var dueClauses []string
	var dueArgs []any
	for i := 0; i+1 < len(telemetrySummaryTiers); i++ {
		tier := telemetrySummaryTiers[i]
		dueClauses = append(dueClauses, "(resolution<? AND bucket+resolution<=?)")
		dueArgs = append(dueArgs, int64(telemetrySummaryTiers[i+1].resolution/time.Second), now.Add(-tier.keep).Unix())
	}
	dueArgs = append(dueArgs, limit)
	rows, err := tx.QueryContext(ctx, t.bind("SELECT series_id,resolution,bucket FROM "+t.table("rollup_due")+" WHERE "+strings.Join(dueClauses, " OR ")+" ORDER BY bucket,series_id,resolution LIMIT ?"), dueArgs...)
	if err != nil {
		return 0, err
	}
	var due []scalarCell
	for rows.Next() {
		var c scalarCell
		if err = rows.Scan(&c.series, &c.resolution, &c.bucket); err != nil {
			break
		}
		due = append(due, c)
	}
	readErr := rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	if readErr != nil {
		return 0, readErr
	}
	done := 0
	for _, d := range due {
		if done >= limit {
			break
		}
		r := time.Duration(d.resolution) * time.Second
		target := legacyRetainedResolution(time.Unix(d.bucket, 0).UnixNano(), r, now)
		if target <= r {
			continue
		}
		rows, err = tx.QueryContext(ctx, t.bind("SELECT bucket,"+scalarStatColumns+" FROM "+t.table("rollup")+" WHERE series_id=? AND resolution=? ORDER BY bucket LIMIT ?"), d.series, d.resolution, limit-done)
		if err != nil {
			return 0, err
		}
		var cells []scalarCell
		for rows.Next() {
			c := scalarCell{series: d.series, resolution: d.resolution}
			if err = rows.Scan(append([]any{&c.bucket}, scalarScan(&c.stats)...)...); err != nil {
				break
			}
			decodeScalarStats(&c.stats, c.bucket)
			cells = append(cells, c)
		}
		readErr = rows.Err()
		rows.Close()
		if err != nil {
			return 0, err
		}
		if readErr != nil {
			return 0, readErr
		}
		for _, c := range cells {
			target = legacyRetainedResolution(time.Unix(c.bucket, 0).UnixNano(), r, now)
			if target <= r {
				break
			}
			parent := scalarCell{c.series, int64(target / time.Second), alignTime(time.Unix(c.bucket, 0), target).Unix(), c.stats}
			if err = t.putScalar(ctx, tx, parent); err != nil {
				return 0, err
			}
			if err = t.fault("tier-parent-before-handoff"); err != nil {
				return 0, err
			}
			if _, err = tx.ExecContext(ctx, t.bind("DELETE FROM "+t.table("rollup")+" WHERE series_id=? AND resolution=? AND bucket=?"), c.series, c.resolution, c.bucket); err != nil {
				return 0, err
			}
			done++
		}
		if err = t.refreshScalarDue(ctx, tx, d.series, d.resolution); err != nil {
			return 0, err
		}
	}
	if err = t.fault("tier-before-commit"); err != nil {
		return 0, err
	}
	return done, tx.Commit()
}

func (t *Telemetry) scalarPromotionDelay(ctx context.Context, now time.Time) (time.Duration, error) {
	rows, err := t.s.reader().QueryContext(ctx, "SELECT resolution,bucket FROM "+t.table("rollup_due"))
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	delay := 24 * time.Hour
	for rows.Next() {
		var seconds, bucket int64
		if err = rows.Scan(&seconds, &bucket); err != nil {
			return 0, err
		}
		r := time.Duration(seconds) * time.Second
		keep := telemetrySummaryKeep(r)
		if keep > 0 {
			due := time.Unix(bucket, 0).Add(r + keep)
			delay = min(delay, max(time.Second, due.Sub(now)))
		}
	}
	return delay, rows.Err()
}

func (t *Telemetry) pruneScalar(ctx context.Context, name string, retention time.Duration, now time.Time, limit int) (int, error) {
	if name == "" || retention <= 0 || limit < 1 || limit > 512 {
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
	rows, err := tx.QueryContext(ctx, t.bind("SELECT b.series_id,b.resolution,b.bucket FROM "+t.table("rollup")+" b JOIN "+t.table("rollup_keys")+" k ON k.id=b.series_id JOIN "+t.table("series")+" s ON s.id=k.source_id WHERE s.metric=? AND b.bucket+b.resolution<=? ORDER BY b.series_id,b.resolution,b.bucket LIMIT ?"), name, now.Add(-retention).Unix(), limit)
	if err != nil {
		return 0, err
	}
	var cells []scalarCell
	for rows.Next() {
		var c scalarCell
		if err = rows.Scan(&c.series, &c.resolution, &c.bucket); err != nil {
			break
		}
		cells = append(cells, c)
	}
	readErr := rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	if readErr != nil {
		return 0, readErr
	}
	for _, c := range cells {
		if _, err = tx.ExecContext(ctx, t.bind("DELETE FROM "+t.table("rollup")+" WHERE series_id=? AND resolution=? AND bucket=?"), c.series, c.resolution, c.bucket); err != nil {
			return 0, err
		}
	}
	seen := map[[2]int64]bool{}
	for _, c := range cells {
		k := [2]int64{c.series, c.resolution}
		if !seen[k] {
			if err = t.refreshScalarDue(ctx, tx, c.series, c.resolution); err != nil {
				return 0, err
			}
			seen[k] = true
		}
	}
	return len(cells), tx.Commit()
}

func (t *Telemetry) aggregateScalar(ctx context.Context, q AggregateQuery, now time.Time, chart bool) (TelemetryQueryResult, error) {
	var out TelemetryQueryResult
	ctx = defaultChartBudget(ctx)
	if err := q.Validate(); err != nil {
		return out, err
	}
	original := q.MetricName
	if original == sqliteVirtualPingLossMetric {
		q.MetricName = sqliteMergedPingLatencyMetric
		q.Aggregation = pingLossPhysicalAggregation(q.Aggregation)
	}
	if !scalarAggregation(q.Aggregation) {
		return out, fmt.Errorf("%w: historical aggregation %s is not retained; available: avg,min,max,sum,count,last", ErrInvalidArgument, q.Aggregation)
	}
	if q.Interval < 10*time.Second || q.Interval%(10*time.Second) != 0 {
		return out, ErrTelemetryCoverage
	}
	tx, shared := ctx.Value(telemetrySnapshotKey{}).(*sql.Tx)
	if !shared {
		var err error
		tx, err = t.s.reader().BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
		if err != nil {
			return out, err
		}
		defer tx.Rollback()
	}
	var err error
	if out.AccountedThrough, err = t.progress(ctx, tx, "summary"); err != nil {
		return out, err
	}
	if out.AcceptedThrough, err = t.progress(ctx, tx, "ingest"); err != nil {
		return out, err
	}
	out.CoverageStart, out.CoverageEnd = q.Start, q.End
	filter, args, err := t.seriesFilter(ctx, q.Query, "s")
	if err != nil {
		return out, err
	}
	statement := "SELECT b.resolution,b.bucket," + "b." + strings.ReplaceAll(scalarStatColumns, ",", ",b.") + ",s.metric,s.entity,s.tags FROM " + t.table("series") + " s JOIN " + t.table("rollup_keys") + " k ON k.source_id=s.id JOIN " + t.table("rollup_due") + " d ON d.series_id=k.id JOIN " + t.table("rollup") + " b ON b.series_id=d.series_id AND b.resolution=d.resolution WHERE s.metric=?" + filter + " AND b.bucket>=? AND b.bucket<=? AND b.bucket+b.resolution>? ORDER BY b.series_id,b.resolution,b.bucket"
	queryArgs := append([]any{q.MetricName}, args...)
	queryArgs = append(queryArgs, q.Start.Add(-24*time.Hour).Unix(), q.End.Unix(), q.Start.Unix())
	rows, err := tx.QueryContext(ctx, t.bind(statement), queryArgs...)
	if err != nil {
		return out, err
	}
	groups := map[aggregateGroupKey]*telemetryStats{}
	tagsByGroup := map[aggregateGroupKey]map[string]string{}
	// Precision is per series/tier, not one manifest object per stored bucket.
	// Keep metadata under the same allocation budget as the chart points.
	type spanKey struct {
		entity, tags, reason string
		resolution           time.Duration
	}
	sources := map[spanKey]TelemetrySourceSpan{}
	span := func(dst map[spanKey]TelemetrySourceSpan, name, entity string, tags map[string]string, first, last time.Time, resolution time.Duration, reason string) error {
		h, _, e := tagsFingerprint(tags)
		if e != nil {
			return e
		}
		k := spanKey{entity, h, reason, resolution}
		v, ok := dst[k]
		if !ok {
			if e = reserveChartMemory(ctx, 0, 384); e != nil {
				return e
			}
			v = TelemetrySourceSpan{MetricName: original, EntityID: entity, Tags: tags, Start: first, End: last, Resolution: resolution, Reason: reason}
		} else {
			v.Start, v.End = minTelemetryTime(v.Start, first), maxTelemetryTime(v.End, last)
		}
		dst[k] = v
		return nil
	}
	card, _ := ctx.Value(telemetryStatisticsCardKey{}).(bool)
	add := func(st telemetryStats, name, entity string, tags map[string]string, resolution time.Duration) error {
		if st.Attempts == 0 {
			return nil
		}
		first, last := time.Unix(0, st.FirstNano), time.Unix(0, st.LastNano)
		if last.Before(q.Start) || first.After(q.End) {
			return nil
		}
		// Sparse coarse buckets can be answered exactly when every retained
		// observation fits one output bucket. Never split a population which
		// actually crosses the requested grid or window.
		if first.Before(q.Start) || last.After(q.End) || (!card && !alignTime(first, q.Interval).Equal(alignTime(last, q.Interval))) {
			out.BoundaryAdjusted = true
			if e := reserveChartMemory(ctx, 0, 384); e != nil {
				return e
			}
			out.Gaps = append(out.Gaps, TelemetrySourceSpan{MetricName: original, EntityID: entity, Tags: tags, Start: maxTelemetryTime(first, q.Start), End: minTelemetryTime(last.Add(time.Nanosecond), q.End.Add(time.Nanosecond)), Resolution: resolution, Reason: "retained bucket crosses requested boundary"})
			return nil
		}
		key, e := telemetryPlanGroup(ctx, q, name, entity, tags, st.FirstNano)
		if e != nil {
			return e
		}
		if groups[key] == nil {
			if e = reserveChartMemory(ctx, 1, 512); e != nil {
				return e
			}
			groups[key] = &telemetryStats{}
			tagsByGroup[key] = tags
		}
		mergeScalarStats(groups[key], st)
		out.Resolution = max(out.Resolution, resolution)
		reason := "scalar rollup"
		if resolution == 0 {
			reason = "unsummarized hot tail"
		}
		return span(sources, name, entity, tags, first, last.Add(time.Nanosecond), resolution, reason)
	}
	lastTags := ""
	var tags map[string]string
	for rows.Next() {
		var st telemetryStats
		var resolution, bucket int64
		var name, entity, raw string
		scan := append([]any{&resolution, &bucket}, scalarScan(&st)...)
		scan = append(scan, &name, &entity, &raw)
		if err = rows.Scan(scan...); err != nil {
			break
		}
		decodeScalarStats(&st, bucket)
		if raw != lastTags || tags == nil {
			var parsed map[string]string
			if err = json.Unmarshal([]byte(raw), &parsed); err != nil {
				break
			}
			tags = parsed
			lastTags = raw
		}
		if err = add(st, name, entity, tags, time.Duration(resolution)*time.Second); err != nil {
			break
		}
	}
	readErr := rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	if readErr != nil {
		return out, readErr
	}
	if chart {
		statement = "SELECT s.metric,s.entity,s.tags,h.observed,h.value,h.ingest_id,h.ordinal FROM " + t.table("hot") + " h JOIN " + t.table("series") + " s ON s.id=h.series_id LEFT JOIN " + t.table("erasures") + " e ON e.series_id=s.id WHERE s.metric=? AND h.ingest_id>? AND h.observed>=? AND h.observed<=? AND (e.through_id IS NULL OR h.ingest_id>e.through_id)" + filter + " ORDER BY h.ingest_id,h.ordinal LIMIT 10001"
		hotArgs := append([]any{q.MetricName, out.AccountedThrough, q.Start.UnixNano(), q.End.UnixNano()}, args...)
		rows, err = tx.QueryContext(ctx, t.bind(statement), hotArgs...)
		if err != nil {
			return out, err
		}
		n := 0
		for rows.Next() {
			n++
			if n > 10000 {
				err = ErrTelemetryBackpressure
				break
			}
			var p Point
			var raw string
			var ns, ingest int64
			var ordinal int
			if err = rows.Scan(&p.MetricName, &p.EntityID, &raw, &ns, &p.Value, &ingest, &ordinal); err != nil {
				break
			}
			p.Timestamp = time.Unix(0, ns)
			if err = json.Unmarshal([]byte(raw), &p.Tags); err != nil {
				break
			}
			var st telemetryStats
			addScalarStats(&st, p, ingest, ordinal)
			if err = add(st, p.MetricName, p.EntityID, p.Tags, 0); err != nil {
				break
			}
		}
		readErr = rows.Err()
		rows.Close()
		if err != nil {
			return out, err
		}
		if readErr != nil {
			return out, readErr
		}
	}
	// One missing contribution makes that series/output bucket unavailable.
	// A later fine delta cannot turn a known partial total into a complete point.
	type missingRange struct{ first, last int64 }
	missing := map[aggregateSeriesKey][]missingRange{}
	for _, gap := range out.Gaps {
		k, e := telemetryPlanGroup(ctx, q, q.MetricName, gap.EntityID, gap.Tags, gap.Start.UnixNano())
		if e != nil {
			return out, e
		}
		missing[k.aggregateSeriesKey] = append(missing[k.aggregateSeriesKey], missingRange{alignTime(gap.Start, q.Interval).UnixNano(), alignTime(gap.End.Add(-time.Nanosecond), q.Interval).UnixNano()})
	}
	for key, ranges := range missing {
		sort.Slice(ranges, func(i, j int) bool { return ranges[i].first < ranges[j].first })
		merged := ranges[:0]
		for _, v := range ranges {
			if len(merged) > 0 && v.first <= merged[len(merged)-1].last {
				merged[len(merged)-1].last = max(merged[len(merged)-1].last, v.last)
			} else {
				merged = append(merged, v)
			}
		}
		missing[key] = merged
	}
	for key := range groups {
		ranges := missing[key.aggregateSeriesKey]
		i := sort.Search(len(ranges), func(i int) bool { return ranges[i].last >= key.bucket })
		if i < len(ranges) && ranges[i].first <= key.bucket {
			delete(groups, key)
		}
	}
	if !chart && len(out.Gaps) > 0 {
		return out, ErrTelemetryCoverage
	}
	out.stats = groups
	keys := make([]aggregateGroupKey, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sortAggregateGroupKeys(keys)
	for _, k := range keys {
		st := groups[k]
		if st.N == 0 && (q.Aggregation == AggAvg || q.Aggregation == AggMin || q.Aggregation == AggMax) {
			continue
		}
		value, e := st.value(q.Aggregation)
		if e != nil {
			return out, e
		}
		entity := q.EntityID
		if q.PreserveSeries {
			entity = k.entityID
		}
		count := st.Attempts
		if q.MetricName == sqliteMergedPingLatencyMetric && original != sqliteVirtualPingLossMetric && q.Aggregation != AggCount {
			count = st.N
		}
		out.Points = append(out.Points, AggregatePoint{MetricName: original, EntityID: entity, Bucket: time.Unix(0, k.bucket).UTC(), Value: value, Count: int(count), Tags: tagsByGroup[k]})
	}
	if q.Order == OrderDesc {
		sort.SliceStable(out.Points, func(i, j int) bool { return out.Points[i].Bucket.After(out.Points[j].Bucket) })
	}
	out.Points = pageBuckets(out.Points, q.BucketLimit, q.BucketOffset)
	for _, v := range sources {
		out.Sources = append(out.Sources, v)
	}
	out.Sources = coalesceTelemetrySpans(out.Sources)
	out.Gaps = coalesceTelemetrySpans(out.Gaps)
	if shared {
		return out, nil
	}
	return out, tx.Commit()
}

func minTelemetryTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func (t *Telemetry) commitScalarLegacyCells(ctx context.Context, key, name, entity string, cells []telemetryMigrationCell) error {
	tx, err := t.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	through, err := t.progress(ctx, tx, "ingest")
	if err != nil {
		return err
	}
	for _, c := range cells {
		if !telemetryChartMetric(name) {
			continue
		}
		source, e := t.seriesKey(ctx, tx, c.series)
		if e != nil {
			return e
		}
		statement := "INSERT INTO " + t.table("series") + " (id,metric,entity,tags) VALUES (?,?,?,?)"
		if t.s.cfg.Driver == DriverMySQL {
			statement += " ON DUPLICATE KEY UPDATE id=id"
		} else {
			statement += " ON CONFLICT(id) DO NOTHING"
		}
		if _, e = tx.ExecContext(ctx, t.bind(statement), source, name, entity, c.tags); e != nil {
			return e
		}
		id, e := t.scalarKey(ctx, tx, source)
		if e != nil {
			return e
		}
		if e = t.putScalar(ctx, tx, scalarCell{id, int64(c.resolution / time.Second), time.Unix(0, c.bucket).Unix(), c.stats}); e != nil {
			return e
		}
		if c.raw.Attempts < c.stats.Attempts {
			if e = t.putScalarRawGap(ctx, tx, id, c.bucket, c.bucket+int64(c.resolution)-1); e != nil {
				return e
			}
		}
	}
	if err = t.setProgress(ctx, tx, "summary", through); err != nil {
		return err
	}
	if err = t.setProgress(ctx, tx, key, 1); err != nil {
		return err
	}
	if err = t.fault("migration-before-window-commit"); err != nil {
		return err
	}
	return tx.Commit()
}

func (t *Telemetry) scalarPingSummary(ctx context.Context, q AggregateQuery, now time.Time) (SeriesSummary, error) {
	ctx = context.WithValue(defaultChartBudget(ctx), telemetryStatisticsCardKey{}, true)
	q.MetricName, q.Aggregation, q.PreserveSeries = sqliteMergedPingLatencyMetric, AggCount, true
	q.Interval = t.compatibleInterval(q.Start, now, max(q.Interval, 10*time.Second))
	r, err := t.Chart(ctx, q, now)
	if err != nil {
		return SeriesSummary{}, err
	}
	if len(r.Gaps) != 0 {
		return SeriesSummary{}, ErrTelemetryCoverage
	}
	var out SeriesSummary
	for _, point := range r.Points {
		h, _, e := tagsFingerprint(point.Tags)
		if e != nil {
			return out, e
		}
		key := aggregateGroupKey{aggregateSeriesKey: aggregateSeriesKey{q.MetricName, point.EntityID, h}}
		st := r.stats[key]
		if st == nil {
			continue
		}
		base := AggregatePoint{MetricName: q.MetricName, EntityID: point.EntityID, Bucket: time.Unix(0, st.LastNano).UTC(), Tags: point.Tags}
		loss := base
		loss.MetricName = sqliteVirtualPingLossMetric
		loss.Count = int(st.Attempts)
		loss.Value = float64(st.Loss) / float64(st.Attempts)
		out.Loss = append(out.Loss, loss)
		if st.N > 0 {
			avg := base
			avg.Count = int(st.N)
			avg.Value = st.Sum / float64(st.N)
			out.Avg = append(out.Avg, avg)
			minimum, maximum := avg, avg
			minimum.Value, maximum.Value = st.Min, st.Max
			out.Min = append(out.Min, minimum)
			out.Max = append(out.Max, maximum)
		}
		if st.Last >= 0 {
			latest := base
			latest.Count = 1
			latest.Value = st.Last
			out.Last = append(out.Last, latest)
		}
	}
	return out, nil
}
