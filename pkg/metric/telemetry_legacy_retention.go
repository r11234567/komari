package metric

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// Imported cells have one canonical owner. Promote and delete together, never
// prune the only copy; online immutable owners use a separate handoff marker.
func (t *Telemetry) PromoteLegacyChunk(ctx context.Context, now time.Time, limit int) (int, error) {
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
	type cell struct {
		key   telemetrySummaryKey
		stats telemetryStats
	}
	var cells []cell
	// Seek each distinct resolution through the existing (resolution,bucket)
	// index, then bound its bucket range before checking segment ownership.
	// A join plus OR and a global time sort rescans every legacy cell per
	// 512-row chunk, making a source-sized migration backlog quadratic.
	for lower := int64(0); lower < int64(24*time.Hour) && len(cells) < limit; {
		var resolution sql.NullInt64
		if err = tx.QueryRowContext(ctx, t.bind("SELECT MIN(resolution) FROM "+t.table("summary")+" WHERE resolution>=? AND resolution<?"), lower, int64(24*time.Hour)).Scan(&resolution); err != nil {
			return 0, err
		}
		if !resolution.Valid {
			break
		}
		r := time.Duration(resolution.Int64)
		if r <= 0 {
			return 0, ErrTelemetryCoverage
		}
		age := telemetrySummaryKeep(r)
		rows, e := tx.QueryContext(ctx, t.bind("SELECT b.segment_id,b.series_id,b.resolution,b.bucket,b.stats FROM "+t.table("summary")+" b WHERE b.resolution=? AND b.bucket<=? AND EXISTS (SELECT 1 FROM "+t.table("segments")+" a WHERE a.id=b.segment_id AND a.state='legacy') ORDER BY b.bucket LIMIT ?"), resolution.Int64, now.Add(-age).UnixNano()-resolution.Int64, limit-len(cells))
		if e != nil {
			return 0, e
		}
		for rows.Next() {
			var c cell
			var raw string
			if e = rows.Scan(&c.key.segment, &c.key.series, &c.key.resolution, &c.key.bucket, &raw); e != nil {
				break
			}
			if e = json.Unmarshal([]byte(raw), &c.stats); e != nil {
				break
			}
			cells = append(cells, c)
		}
		re := rows.Err()
		rows.Close()
		if e != nil {
			return 0, e
		}
		if re != nil {
			return 0, re
		}
		lower = resolution.Int64 + 1
	}
	for _, c := range cells {
		k := c.key
		r := legacyRetainedResolution(k.bucket, time.Duration(k.resolution), now)
		bucket := alignTime(time.Unix(0, k.bucket).UTC(), r).UnixNano()
		var raw string
		var prior telemetryStats
		err = tx.QueryRowContext(ctx, t.bind("SELECT stats FROM "+t.table("summary")+" WHERE segment_id=? AND series_id=? AND resolution=? AND bucket=?"), k.segment, k.series, int64(r), bucket).Scan(&raw)
		if err == nil {
			if err = json.Unmarshal([]byte(raw), &prior); err != nil {
				return 0, err
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
		if err = prior.merge(c.stats); err != nil {
			return 0, err
		}
		encoded, err := prior.encodeSealed()
		if err != nil {
			return 0, err
		}
		statement := "INSERT INTO " + t.table("summary") + " (segment_id,series_id,resolution,bucket,stats) VALUES (?,?,?,?,?)"
		if t.s.cfg.Driver == DriverMySQL {
			statement += " ON DUPLICATE KEY UPDATE stats=VALUES(stats)"
		} else {
			statement += " ON CONFLICT(segment_id,series_id,resolution,bucket) DO UPDATE SET stats=excluded.stats"
		}
		if _, err = tx.ExecContext(ctx, t.bind(statement), k.segment, k.series, int64(r), bucket, encoded); err != nil {
			return 0, err
		}
		if _, err = tx.ExecContext(ctx, t.bind("DELETE FROM "+t.table("summary")+" WHERE segment_id=? AND series_id=? AND resolution=? AND bucket=?"), k.segment, k.series, k.resolution, k.bucket); err != nil {
			return 0, err
		}
	}
	if err = t.fault("legacy-before-promotion-commit"); err != nil {
		return 0, err
	}
	return len(cells), tx.Commit()
}

// Use the same inclusive bucket-end boundaries as the promotion selector.
// Looking up telemetryResolution(age) at an exact boundary would select the
// source tier again, upsert into its own key, then delete that only copy.
func legacyRetainedResolution(bucket int64, resolution time.Duration, now time.Time) time.Duration {
	end := bucket + int64(resolution)
	target := telemetrySummaryTiers[0].resolution
	for i := 0; i+1 < len(telemetrySummaryTiers); i++ {
		if end <= now.Add(-telemetrySummaryTiers[i].keep).UnixNano() {
			target = telemetrySummaryTiers[i+1].resolution
		}
	}
	return telemetryLCM(resolution, target)
}
