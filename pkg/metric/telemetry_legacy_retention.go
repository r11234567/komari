package metric

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// Imported cells have one canonical owner, unlike online deltas which already
// contain every tier. Promote and delete together, never prune the only copy.
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
	rows, err := tx.QueryContext(ctx, t.bind("SELECT b.segment_id,b.series_id,b.resolution,b.bucket,b.stats FROM "+t.table("summary")+" b JOIN "+t.table("segments")+" a ON a.id=b.segment_id WHERE a.state='legacy' AND ((b.resolution<? AND b.bucket+b.resolution<=?) OR (b.resolution<? AND b.bucket+b.resolution<=?) OR (b.resolution<? AND b.bucket+b.resolution<=?)) ORDER BY b.bucket,b.segment_id,b.series_id LIMIT ?"), int64(time.Minute), now.Add(-3*time.Hour).UnixNano(), int64(15*time.Minute), now.Add(-48*time.Hour).UnixNano(), int64(time.Hour), now.Add(-14*24*time.Hour).UnixNano(), limit)
	if err != nil {
		return 0, err
	}
	type cell struct {
		key   telemetrySummaryKey
		stats telemetryStats
	}
	var cells []cell
	for rows.Next() {
		var c cell
		var raw string
		if err = rows.Scan(&c.key.segment, &c.key.series, &c.key.resolution, &c.key.bucket, &raw); err != nil {
			break
		}
		if err = json.Unmarshal([]byte(raw), &c.stats); err != nil {
			break
		}
		cells = append(cells, c)
	}
	re := rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	if re != nil {
		return 0, re
	}
	for _, c := range cells {
		k := c.key
		r := telemetryLCM(time.Duration(k.resolution), telemetryResolution(now.Sub(time.Unix(0, k.bucket+k.resolution))))
		bucket := time.Unix(0, k.bucket).UTC().Truncate(r).UnixNano()
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
		encoded, err := prior.encode()
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
