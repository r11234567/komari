package metric

// Legacy codecs are used only by this explicit offline reader. Import writes
// ordinary hot rows and independent summary cells; it never seals, recompresses
// or deletes a source block. Original tables remain a rollback source.
import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"
)

type telemetryMigrationCell struct {
	series, tags string
	bucket       int64
	resolution   time.Duration
	stats        telemetryStats
	raw, extra   telemetryStats
}

func telemetryMigrationKey(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}
func (t *Telemetry) setProgress(ctx context.Context, tx *sql.Tx, key string, value int64) error {
	q := "INSERT INTO " + t.table("progress") + " (task,value) VALUES (?,?)"
	if t.s.cfg.Driver == DriverMySQL {
		q += " ON DUPLICATE KEY UPDATE value=VALUES(value)"
	} else {
		q += " ON CONFLICT(task) DO UPDATE SET value=excluded.value"
	}
	_, err := tx.ExecContext(ctx, t.bind(q), key, value)
	return err
}

// MigrateLegacy is intentionally unavailable to HTTP ingestion. Call with the
// service stopped, after a complete backup. Each metric/entity/3h window has a
// durable completion marker; acknowledged input and summaries survive restart.
func (s *Store) MigrateLegacy(ctx context.Context, cfg TelemetryConfig, observe func(string, int64)) error {
	if s.Telemetry() != nil {
		return errors.New("migration requires an offline legacy reader")
	}
	checkpoint, err := s.PrepareLegacyMigration(ctx, cfg)
	if err != nil {
		return err
	}
	t, err := s.OpenTelemetry(ctx, cfg)
	if err != nil {
		return err
	}
	defer t.Close()
	now := checkpoint.Cutoff
	if checkpoint.Complete {
		if err = t.finishLegacyMigrationRetention(ctx, now); err != nil {
			return err
		}
		if err = t.finishLegacyRawHandoff(ctx); err != nil {
			return err
		}
		_, err = t.s.db.ExecContext(ctx, "DELETE FROM "+t.table("progress")+" WHERE task LIKE 'm/%'")
		if err != nil {
			return err
		}
		return t.finishLegacyReceipts(ctx)
	}
	defs, err := s.ListMetrics(ctx)
	if err != nil {
		return err
	}
	var globalStart sql.NullInt64
	tables := []struct{ table, column string }{{s.tables.points, "ts_nano"}, {s.tables.rollups, "bucket_nano"}}
	if s.sqliteStorageV4 {
		tables = []struct{ table, column string }{{s.tables.pointValues, "ts_nano"}, {s.tables.pointBlocks, "start_nano"}, {s.tables.rollupValues, "bucket_nano"}, {s.tables.rollupBlocks, "start_nano"}}
	} else if s.externalPointBlocks {
		tables = append(tables, struct{ table, column string }{s.tables.pointBlocks, "start_nano"})
	}
	resolutions := map[time.Duration]bool{}
	for _, table := range tables {
		var first sql.NullInt64
		if err = s.reader().QueryRowContext(ctx, "SELECT MIN("+table.column+") FROM "+table.table).Scan(&first); err != nil {
			return err
		}
		if first.Valid && (!globalStart.Valid || first.Int64 < globalStart.Int64) {
			globalStart = first
		}
		if table.column == "bucket_nano" || table.table == s.tables.rollupBlocks {
			// Old retention can leave obsolete tiers in the source. Only rows
			// intersecting a metric's migration range constrain this import.
			// Include the complete last bucket when testing overlap so a coarse
			// retained boundary cannot be mistaken for expired history.
			statement := "SELECT DISTINCT r.resolution_nano FROM " + table.table + " r"
			metricColumn := "r.metric_name"
			if s.sqliteStorageV4 {
				statement += " JOIN " + s.tables.series + " s ON s.id=r.series_id"
				metricColumn = "s.metric_name"
			}
			first, last := "r."+table.column, "r."+table.column
			if table.table == s.tables.rollupBlocks {
				last = "r.end_nano"
			}
			statement += " WHERE " + metricColumn + "=? AND (" + last + "+r.resolution_nano)>? AND " + first + "<=? LIMIT 33"
			for _, def := range defs {
				if def.RetentionDays <= 0 || def.Name == sqliteVirtualPingLossMetric {
					continue
				}
				from := now.Add(-time.Duration(def.RetentionDays) * 24 * time.Hour).Truncate(3 * time.Hour)
				until := now.Truncate(3 * time.Hour).Add(3*time.Hour - time.Nanosecond)
				rows, e := s.reader().QueryContext(ctx, t.bind(statement), def.Name, from.UnixNano(), until.UnixNano())
				if e != nil {
					return e
				}
				for rows.Next() {
					var n int64
					if e = rows.Scan(&n); e != nil {
						break
					}
					if n <= 0 || 3*int64(time.Hour)%n != 0 {
						e = fmt.Errorf("unsupported legacy interval %d; preserve source and request a migration adapter", n)
						break
					}
					resolutions[time.Duration(n)] = true
				}
				re := rows.Err()
				rows.Close()
				if e != nil {
					return e
				}
				if re != nil {
					return re
				}
				if len(resolutions) > 32 {
					return ErrTelemetryBackpressure
				}
			}
		}
	}
	var tiers []time.Duration
	for r := range resolutions {
		tiers = append(tiers, r)
	}
	sort.Slice(tiers, func(i, j int) bool { return tiers[i] > tiers[j] })
	legacyResolution := time.Second * 10
	// Kept only as a backwards-compatible migration marker. Source cells own
	// their actual resolution; no global LCM/hour floor controls chart queries.
	if globalStart.Valid {
		for _, def := range defs {
			if def.RetentionDays <= 0 || def.Name == sqliteVirtualPingLossMetric {
				continue
			}
			start := maxTelemetryTime(time.Unix(0, globalStart.Int64).UTC(), now.Add(-time.Duration(def.RetentionDays)*24*time.Hour)).Truncate(3 * time.Hour)
			wide := Query{MetricName: def.Name, Start: start, End: now.Add(5 * time.Minute)}
			entities, e := s.EntityIDs(ctx, wide)
			if e != nil {
				return e
			}
			for _, entity := range entities {
				for from := start; !from.After(now); from = from.Add(3 * time.Hour) {
					end := from.Add(3*time.Hour - time.Nanosecond)
					key := "m/" + telemetryMigrationKey(def.Name + "\x00" + entity + "\x00" + from.Format(time.RFC3339Nano))[:60]
					if done, e := t.progress(ctx, s.db, key); e == nil && done == 1 {
						continue
					} else if e != nil && !errors.Is(e, sql.ErrNoRows) {
						return e
					}
					cells, e := s.legacyMigrationCells(ctx, def.Name, entity, from, end, tiers)
					if e != nil {
						return e
					}
					count, e := s.migrateLegacyRawWindow(ctx, t, def.Name, entity, from, end, now, key, &cells)
					if e != nil {
						return e
					}
					// An offline source cannot acquire new rows in an empty range.
					// Rechecking it after an interrupted import is cheap; committing
					// thousands of empty markers and running archive/GC per gap is
					// not. Keep durable checkpoints only for actual source input.
					if count == 0 && len(cells) == 0 {
						continue
					}
					cells, e = coarsenLegacyMigrationCells(cells, now)
					if e != nil {
						return e
					}
					if e = t.commitLegacyCells(ctx, key, def.Name, entity, cells); e != nil {
						return e
					}
					if observe != nil {
						observe(def.Name+"/"+entity, count)
					}
					// Bound hot ownership even while an incident-sized source is read.
					// Historical observation time is not a wall-clock flush timer.
					// Keep each machine/day open until its byte budget is filled;
					// sealing every sparse 3h metric window creates tiny files.
					if _, e = t.s.db.ExecContext(ctx, t.bind("UPDATE "+t.table("segments")+" SET state='sealed' WHERE state='open' AND (points>=? OR bytes>=? OR (SELECT value FROM "+t.table("progress")+" WHERE task='hot_bytes')>=?)"), t.cfg.SegmentPoints, t.cfg.SegmentBytes, t.cfg.MaxHotBytes/2); e != nil {
						return e
					}
					for {
						ok, e := t.ArchiveNext(ctx)
						if e != nil {
							return e
						}
						if !ok {
							break
						}
					}
					for {
						n, e := t.DeleteArchivedChunk(ctx, now, 512)
						if e != nil {
							return e
						}
						if n == 0 {
							break
						}
					}
				}
			}
		}
	}
	// A restart may retain windows committed by an older importer. Converge
	// those cells before publishing completion instead of handing a source-
	// sized fine-tier backlog to the first online scheduler/query requests.
	if err = t.finishLegacyMigrationRetention(ctx, now); err != nil {
		return err
	}
	if err = t.finishLegacyRawHandoff(ctx); err != nil {
		return err
	}
	tx, err := t.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Window checkpoints are needed only until the global completion marker.
	// Retire them in that same commit, so interruption cannot re-add population.
	if _, err = tx.ExecContext(ctx, "DELETE FROM "+t.table("progress")+" WHERE task LIKE 'm/%'"); err != nil {
		return err
	}
	for key, value := range map[string]int64{"legacy_migration_complete": 1, "legacy_until": now.UnixNano(), "legacy_resolution": int64(legacyResolution)} {
		if err = t.setProgress(ctx, tx, key, value); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	return t.finishLegacyReceipts(ctx)
}

func (t *Telemetry) finishLegacyReceipts(ctx context.Context) error {
	for {
		n, err := t.PruneReceipts(ctx, time.Now().UTC(), 512)
		if err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
	}
}

// The service is stopped, so the last short segment has no future writer.
// Publish it as well before declaring migration complete. A completed-window
// checkpoint must not leave the final raw SQL tail stranded across restarts.
func (t *Telemetry) finishLegacyRawHandoff(ctx context.Context) error {
	for {
		n, err := t.SummarizeNext(ctx)
		if err != nil {
			return err
		}
		if n == 0 {
			break
		}
	}
	if _, err := t.s.db.ExecContext(ctx, "UPDATE "+t.table("segments")+" SET state='sealed' WHERE state='open'"); err != nil {
		return err
	}
	for {
		ok, err := t.ArchiveNext(ctx)
		if err != nil {
			return err
		}
		if !ok {
			break
		}
	}
	for {
		n, err := t.DeleteArchivedChunk(ctx, time.Now().UTC(), 512)
		if err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
	}
}

func coarsenLegacyMigrationCells(cells []telemetryMigrationCell, now time.Time) ([]telemetryMigrationCell, error) {
	sort.Slice(cells, func(i, j int) bool {
		if cells[i].series != cells[j].series {
			return cells[i].series < cells[j].series
		}
		return cells[i].bucket < cells[j].bucket
	})
	groups := make(map[telemetrySummaryKey]int)
	var out []telemetryMigrationCell
	for _, c := range cells {
		r := legacyRetainedResolution(c.bucket, c.resolution, now)
		key := telemetrySummaryKey{series: c.series, resolution: int64(r), bucket: alignTime(time.Unix(0, c.bucket).UTC(), r).UnixNano()}
		index, ok := groups[key]
		if !ok {
			groups[key] = len(out)
			next := telemetryMigrationCell{series: c.series, tags: c.tags, bucket: key.bucket, resolution: r}
			// stats/raw often share the original raw sketch. Seed through merge
			// so subsequent statistics merging never mutates the raw proof too.
			if err := next.stats.merge(c.stats); err != nil {
				return nil, err
			}
			if err := next.raw.merge(c.raw); err != nil {
				return nil, err
			}
			out = append(out, next)
			continue
		}
		if err := out[index].stats.merge(c.stats); err != nil {
			return nil, err
		}
		if err := out[index].raw.merge(c.raw); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (t *Telemetry) finishLegacyMigrationRetention(ctx context.Context, now time.Time) error {
	if done, err := t.progress(ctx, t.s.db, "legacy_retention_policy"); err == nil && done == 2 {
		return nil
	} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	for {
		n, err := t.PromoteLegacyChunk(ctx, now, 512)
		if err != nil {
			return err
		}
		if n == 0 {
			break
		}
	}
	tx, err := t.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = t.setProgress(ctx, tx, "legacy_retention_complete", 1); err != nil {
		return err
	}
	if err = t.setProgress(ctx, tx, "legacy_retention_policy", 2); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) legacyMigrationCells(ctx context.Context, name, entity string, start, end time.Time, tiers []time.Duration) ([]telemetryMigrationCell, error) {
	var out []telemetryMigrationCell
	for _, resolution := range tiers {
		sort.Slice(out, func(i, j int) bool {
			if out[i].series != out[j].series {
				return out[i].series < out[j].series
			}
			return out[i].bucket < out[j].bucket
		})
		refinements := map[int][]telemetryMigrationCell{}
		var additional []telemetryMigrationCell
		rows, err := s.scanRollupRowsBetween(ctx, name, entity, nil, int64(resolution), start.UnixNano(), end.UnixNano(), false)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			b := row.bucketData
			tags, err := decodeMapString(b.tagsJSON)
			if err != nil {
				return nil, err
			}
			tagBytes, _ := json.Marshal(tags)
			sid := telemetryMigrationKey(name + "\x00" + entity + "\x00" + string(tagBytes))
			overlap := -1
			alternate := false
			for i, c := range out {
				if c.series == sid && row.bucket < c.bucket+int64(c.resolution) && row.bucket+int64(resolution) > c.bucket {
					if row.bucket < c.bucket || row.bucket+int64(resolution) > c.bucket+int64(c.resolution) {
						// Legacy 45m and 1h tiers are alternate representations
						// but are not nested. Keep the selected coarse population
						// when it covers every observed part of this alternative.
						// An uncovered extension still needs a migration adapter.
						if legacyMigrationObservedRangeCovered(out, sid, b.firstTS, b.lastTS) {
							alternate = true
							break
						}
						return nil, fmt.Errorf("%w: partially overlapping legacy tiers for %s/%s; source retained", ErrTelemetryCoverage, name, entity)
					}
					overlap = i
					break
				}
			}
			if alternate {
				continue
			}
			n := b.count - b.lossCount
			st := telemetryStats{N: n, Attempts: b.count, Loss: b.lossCount, Sum: b.sum, Min: b.min, Max: b.max, First: b.firstVal, Last: b.lastVal, FirstNano: b.firstTS, LastNano: b.lastTS}
			if n > 0 {
				st.Mean = b.sum / float64(n)
			}
			cell := telemetryMigrationCell{series: sid, tags: string(tagBytes), bucket: row.bucket, resolution: resolution, stats: st}
			if overlap >= 0 {
				refinements[overlap] = append(refinements[overlap], cell)
			} else {
				additional = append(additional, cell)
			}
			if len(out)+len(additional) > 16384 {
				return nil, ErrTelemetryBackpressure
			}
		}
		// Keep fine retained history only when its complete population proves
		// equivalence to the coarse representation. Partial fine retention may
		// not delete or double-count the older coarse population.
		var refined []telemetryMigrationCell
		for i, parent := range out {
			children := refinements[i]
			var combined telemetryStats
			for _, child := range children {
				if err := combined.merge(child.stats); err != nil {
					return nil, err
				}
			}
			if len(children) > 0 && equivalentLegacyPopulation(parent.stats, combined) {
				refined = append(refined, children...)
			} else {
				refined = append(refined, parent)
			}
		}
		if len(refined)+len(additional) > 16384 {
			return nil, ErrTelemetryBackpressure
		}
		out = append(refined, additional...)
	}
	return out, nil
}

func legacyMigrationObservedRangeCovered(cells []telemetryMigrationCell, series string, first, last int64) bool {
	if first > last {
		return false
	}
	index := sort.Search(len(cells), func(i int) bool { return cells[i].series >= series })
	cursor := first
	for ; index < len(cells) && cells[index].series == series; index++ {
		cell := cells[index]
		if cell.stats.Attempts == 0 {
			continue
		}
		if cell.bucket > cursor {
			return false
		}
		cursor = max(cursor, cell.bucket+int64(cell.resolution))
		if cursor > last {
			return true
		}
	}
	return false
}

func (s *Store) migrateLegacyRawWindow(ctx context.Context, t *Telemetry, name, entity string, start, end, now time.Time, key string, cells *[]telemetryMigrationCell) (int64, error) {
	var total int64
	outside := map[telemetrySummaryKey]*telemetryMigrationCell{}
	inside := map[int]map[telemetrySummaryKey]*telemetryMigrationCell{}
	insideCount := 0
	batchIndex := 0
	err := s.visitLegacyMigrationRaw(ctx, t, name, entity, start, end, func(points []Point) error {
		// Use one import clock for segment lifetime. Advancing the receipt
		// clock through historical windows would expire a sparse segment on
		// every window, even though only milliseconds of import work elapsed.
		if _, err := t.accept(ctx, TelemetryBatch{Source: entity, Identity: fmt.Sprintf("legacy/%s/%d", key, batchIndex), Points: points}, now, true); err != nil {
			return err
		}
		batchIndex++
		for _, p := range points {
			tags, _ := json.Marshal(p.normalized().Tags)
			sid := telemetryMigrationKey(name + "\x00" + entity + "\x00" + string(tags))
			found := false
			for i := range *cells {
				c := &(*cells)[i]
				if c.series == sid && p.Timestamp.UnixNano() >= c.bucket && p.Timestamp.UnixNano() < c.bucket+int64(c.resolution) {
					c.raw.add(p, total+1, 0)
					r := telemetryResolution(now.Sub(p.Timestamp))
					// Partition every raw point, including equal-tier points on a
					// retention boundary. A 45m source also needs rebucketing onto
					// 1h from raw, rather than being relabelled with its 3h LCM.
					k := telemetrySummaryKey{series: sid, resolution: int64(r), bucket: p.Timestamp.Truncate(r).UnixNano()}
					if inside[i] == nil {
						inside[i] = map[telemetrySummaryKey]*telemetryMigrationCell{}
					}
					if inside[i][k] == nil {
						inside[i][k] = &telemetryMigrationCell{series: sid, tags: string(tags), bucket: k.bucket, resolution: r}
						insideCount++
					}
					inside[i][k].stats.add(p, total+1, 0)
					if insideCount > 16384 {
						return ErrTelemetryBackpressure
					}
					if p.Timestamp.UnixNano() > c.stats.LastNano {
						c.extra.add(p, total+1, 0)
					}
					found = true
					break
				}
			}
			if !found {
				r := telemetryResolution(now.Sub(p.Timestamp))
				k := telemetrySummaryKey{series: sid, resolution: int64(r), bucket: p.Timestamp.Truncate(r).UnixNano()}
				if outside[k] == nil {
					outside[k] = &telemetryMigrationCell{series: sid, tags: string(tags), bucket: k.bucket, resolution: r}
				}
				outside[k].stats.add(p, total+1, 0)
				if len(outside) > 16384 {
					return ErrTelemetryBackpressure
				}
			}
			total++
		}
		return nil
	})
	if err != nil {
		return total, err
	}
	var refined []telemetryMigrationCell
	for i := range *cells {
		c := &(*cells)[i]
		rawOwnsPopulation := c.raw.Attempts >= c.stats.Attempts
		if rawOwnsPopulation {
			if c.raw.FirstNano > c.stats.FirstNano || c.raw.LastNano < c.stats.LastNano {
				return total, fmt.Errorf("%w: raw population does not cover legacy range; source retained", ErrTelemetryCoverage)
			}
			c.stats = c.raw
		} else if c.extra.Attempts > 0 {
			// The old summary has no input watermark. A partial raw overlap
			// cannot prove which rows it already contains. Never guess a union.
			return total, fmt.Errorf("%w: ambiguous raw/rollup overlap for %s/%s; source retained", ErrTelemetryCoverage, name, entity)
		}
		var children []telemetryMigrationCell
		var combined telemetryStats
		for _, child := range inside[i] {
			child.raw = child.stats
			children = append(children, *child)
		}
		sort.Slice(children, func(i, j int) bool { return children[i].bucket < children[j].bucket })
		for _, child := range children {
			if err := combined.merge(child.stats); err != nil {
				return total, err
			}
		}
		// These children were built from the exact same visited raw points as
		// c.raw. Independently compressed digests need not have identical
		// centroids after partition/merge; that is not evidence of missing raw.
		if len(children) > 0 && (rawOwnsPopulation || equivalentLegacyPopulation(c.stats, combined)) {
			refined = append(refined, children...)
		} else {
			refined = append(refined, *c)
		}
	}
	*cells = refined
	for _, c := range outside {
		c.raw = c.stats
		*cells = append(*cells, *c)
	}
	return total, nil
}

func equivalentLegacyPopulation(a, b telemetryStats) bool {
	return a.Attempts == b.Attempts && a.N == b.N && a.Loss == b.Loss &&
		a.FirstNano == b.FirstNano && a.LastNano == b.LastNano && a.First == b.First && a.Last == b.Last &&
		a.Min == b.Min && a.Max == b.Max && math.Abs(a.Sum-b.Sum) <= 1e-10*max(1, math.Abs(a.Sum))
}

// Scalar moments alone do not identify a population. Compare the retained
// distribution as well; when independently compressed sketches cannot prove
// equivalence we conservatively keep the original coarse statistics.
func equivalentLegacyDigest(a, b telemetryStats) bool {
	decode := func(s telemetryStats) *TDigest {
		if s.sketch != nil {
			d, _ := DecodeTDigest(s.sketch.encodeRaw())
			return d
		}
		d, _ := DecodeTDigest(s.Digest)
		return d
	}
	x, y := decode(a), decode(b)
	if x == nil || y == nil {
		return a.N == 0 && b.N == 0
	}
	x.process()
	y.process()
	if x.count != y.count || len(x.centroids) != len(y.centroids) {
		return false
	}
	for i, c := range x.centroids {
		d := y.centroids[i]
		if c.weight != d.weight || math.Abs(c.mean-d.mean) > 1e-10*max(1, math.Abs(c.mean), math.Abs(d.mean)) {
			return false
		}
	}
	return true
}

func (t *Telemetry) commitLegacyCells(ctx context.Context, key, name, entity string, cells []telemetryMigrationCell) error {
	if t.scalar {
		return t.commitScalarLegacyCells(ctx, key, name, entity, cells)
	}
	tx, err := t.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	through, err := t.progress(ctx, tx, "ingest")
	if err != nil {
		return err
	}
	segment := telemetryMigrationKey(key)[:32]
	q := "INSERT INTO " + t.table("segments") + " (id,state,first_id,last_id,points,bytes,created,deleted,checksum,file_bytes,staging_owner) VALUES (?,'legacy',1,?,0,0,0,0,'',0,'')"
	if t.s.cfg.Driver == DriverMySQL {
		q += " ON DUPLICATE KEY UPDATE last_id=VALUES(last_id)"
	} else {
		q += " ON CONFLICT(id) DO UPDATE SET last_id=excluded.last_id"
	}
	if _, err = tx.ExecContext(ctx, t.bind(q), segment, max(1, through)); err != nil {
		return err
	}
	cells = append([]telemetryMigrationCell(nil), cells...)
	resolvedKeys := make(map[string]string)
	for i := range cells {
		canonical := cells[i].series
		sid, ok := resolvedKeys[canonical]
		if !ok {
			sid, err = t.seriesKey(ctx, tx, canonical)
			if err != nil {
				return err
			}
			resolvedKeys[canonical] = sid
		}
		cells[i].series = sid
	}
	for _, c := range cells {
		if c.raw.Attempts < c.stats.Attempts {
			if _, err = tx.ExecContext(ctx, t.bind("INSERT INTO "+t.table("gaps")+" (segment_id,series_id,first_observed,last_observed) VALUES (?,?,?,?)"), segment, c.series, c.stats.FirstNano, c.stats.LastNano); err != nil {
				return err
			}
		}
		// The source dictionary is kept in the same offline Store. Imported
		// rollup-only series may not have any raw row, so create their identity.
		seriesSQL := "INSERT INTO " + t.table("series") + " (id,metric,entity,tags) VALUES (?,?,?,?)"
		if t.s.cfg.Driver == DriverMySQL {
			seriesSQL += " ON DUPLICATE KEY UPDATE id=id"
		} else {
			seriesSQL += " ON CONFLICT(id) DO NOTHING"
		}
		if _, err = tx.ExecContext(ctx, t.bind(seriesSQL), c.series, name, entity, c.tags); err != nil {
			return err
		}
		encoded, e := c.stats.encodeSealed()
		if e != nil {
			return e
		}
		if _, err = tx.ExecContext(ctx, t.bind("INSERT INTO "+t.table("summary")+" (segment_id,series_id,resolution,bucket,stats) VALUES (?,?,?,?,?)"), segment, c.series, int64(c.resolution), c.bucket, encoded); err != nil {
			return err
		}
	}
	bySeries := map[string][]int64{}
	for _, c := range cells {
		v := bySeries[c.series]
		if v == nil {
			v = []int64{c.stats.FirstNano, c.stats.LastNano, c.stats.Attempts}
		} else {
			v[0] = min(v[0], c.stats.FirstNano)
			v[1] = max(v[1], c.stats.LastNano)
			v[2] += c.stats.Attempts
		}
		bySeries[c.series] = v
	}
	for sid, v := range bySeries {
		if _, err = tx.ExecContext(ctx, t.bind("INSERT INTO "+t.table("members")+" (segment_id,series_id,first_observed,last_observed,points) VALUES (?,?,?,?,?)"), segment, sid, v[0], v[1], v[2]); err != nil {
			return err
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

func telemetryLCM(a, b time.Duration) time.Duration {
	if a <= 0 {
		return b
	}
	if b <= 0 {
		return a
	}
	x, y := a, b
	for y != 0 {
		x, y = y, x%y
	}
	if int64(a/x) > math.MaxInt64/int64(b) {
		return time.Duration(math.MaxInt64)
	}
	return a / x * b
}
