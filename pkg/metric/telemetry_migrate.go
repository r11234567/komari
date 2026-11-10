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
	t, err := s.OpenTelemetry(ctx, cfg)
	if err != nil {
		return err
	}
	defer t.Close()
	if v, e := t.progress(ctx, s.db, "legacy_migration_complete"); e == nil && v == 1 {
		return nil
	}
	accepted, err := t.progress(ctx, s.db, "ingest")
	if err != nil {
		return err
	}
	started, e := t.progress(ctx, s.db, "legacy_migration_started")
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return e
	}
	if accepted > 0 && started != 1 {
		return errors.New("target already contains online telemetry; refusing an ambiguous offline merge")
	}
	tx, err := t.begin(ctx)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	if epoch, e := t.progress(ctx, tx, "legacy_migration_now"); e == nil {
		now = time.Unix(0, epoch).UTC()
	} else if !errors.Is(e, sql.ErrNoRows) {
		tx.Rollback()
		return e
	}
	if err = t.setProgress(ctx, tx, "legacy_migration_now", now.UnixNano()); err != nil {
		tx.Rollback()
		return err
	}
	if err = t.setProgress(ctx, tx, "legacy_migration_started", 1); err != nil {
		tx.Rollback()
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
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
			rows, e := s.reader().QueryContext(ctx, "SELECT DISTINCT resolution_nano FROM "+table.table+" LIMIT 33")
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
	var tiers []time.Duration
	for r := range resolutions {
		tiers = append(tiers, r)
	}
	sort.Slice(tiers, func(i, j int) bool { return tiers[i] > tiers[j] })
	legacyResolution := time.Second * 10
	for _, r := range tiers {
		legacyResolution = telemetryLCM(legacyResolution, r)
	}
	// Imported raw-only windows already use the age-appropriate retained tier.
	legacyResolution = telemetryLCM(legacyResolution, time.Hour)
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
					if e = t.commitLegacyCells(ctx, key, def.Name, entity, cells); e != nil {
						return e
					}
					if observe != nil {
						observe(def.Name+"/"+entity, count)
					}
					// Bound hot ownership even while an incident-sized source is read.
					if _, e = t.SealDue(ctx, now); e != nil {
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
	tx, err = t.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for key, value := range map[string]int64{"legacy_migration_complete": 1, "legacy_until": now.UnixNano(), "legacy_resolution": int64(legacyResolution)} {
		if err = t.setProgress(ctx, tx, key, value); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) legacyMigrationCells(ctx context.Context, name, entity string, start, end time.Time, tiers []time.Duration) ([]telemetryMigrationCell, error) {
	var out []telemetryMigrationCell
	for _, resolution := range tiers {
		rows, err := s.scanRollupRowsBetween(ctx, name, entity, nil, int64(resolution), start.UnixNano(), end.UnixNano(), true)
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
			overlaps := false
			for _, c := range out {
				if c.series == sid && row.bucket < c.bucket+int64(c.resolution) && row.bucket+int64(resolution) > c.bucket {
					if row.bucket < c.bucket || row.bucket+int64(resolution) > c.bucket+int64(c.resolution) {
						return nil, fmt.Errorf("%w: partially overlapping legacy tiers for %s/%s; source retained", ErrTelemetryCoverage, name, entity)
					}
					overlaps = true
					break
				}
			}
			if overlaps {
				continue
			}
			n := b.count - b.lossCount
			st := telemetryStats{N: n, Attempts: b.count, Loss: b.lossCount, Sum: b.sum, Min: b.min, Max: b.max, First: b.firstVal, Last: b.lastVal, FirstNano: b.firstTS, LastNano: b.lastTS}
			if n > 0 {
				st.Mean = b.sum / float64(n)
				st.M2 = max(0, b.sumSq-b.sum*b.sum/float64(n))
			}
			if b.digest != nil {
				st.Digest = b.digest.encodeRaw()
			}
			out = append(out, telemetryMigrationCell{series: sid, tags: string(tagBytes), bucket: row.bucket, resolution: resolution, stats: st})
			if len(out) > 16384 {
				return nil, ErrTelemetryBackpressure
			}
		}
	}
	return out, nil
}

func (s *Store) migrateLegacyRawWindow(ctx context.Context, t *Telemetry, name, entity string, start, end, now time.Time, key string, cells *[]telemetryMigrationCell) (int64, error) {
	var total int64
	outside := map[telemetrySummaryKey]*telemetryMigrationCell{}
	batchIndex := 0
	err := s.visitLegacyMigrationRaw(ctx, t, name, entity, start, end, func(points []Point) error {
		received := end
		if received.After(now) {
			received = now
		}
		if _, err := t.accept(ctx, TelemetryBatch{Source: entity, Identity: fmt.Sprintf("legacy/%s/%d", key, batchIndex), Points: points}, received, true); err != nil {
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
	for i := range *cells {
		c := &(*cells)[i]
		if c.raw.Attempts >= c.stats.Attempts {
			c.stats = c.raw
		} else if c.extra.Attempts > 0 {
			// The old summary has no input watermark. A partial raw overlap
			// cannot prove which rows it already contains. Never guess a union.
			return total, fmt.Errorf("%w: ambiguous raw/rollup overlap for %s/%s; source retained", ErrTelemetryCoverage, name, entity)
		}
	}
	for _, c := range outside {
		c.raw = c.stats
		*cells = append(*cells, *c)
	}
	return total, nil
}

func (t *Telemetry) commitLegacyCells(ctx context.Context, key, name, entity string, cells []telemetryMigrationCell) error {
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
		encoded, e := c.stats.encode()
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
