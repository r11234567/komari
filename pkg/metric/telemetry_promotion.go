package metric

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

var telemetryPromotionTiers = [...]struct{ source, target time.Duration }{
	{10 * time.Second, time.Minute}, {time.Minute, 15 * time.Minute}, {15 * time.Minute, time.Hour},
}

// PromoteSummariesChunk visits bounded closed buckets, never every report or
// every historical bucket. A sealed segment's last_id is its immutable input
// identity: later observations get another segment, including late timestamps.
// Parent statistics, coverage and child handoff are one transaction. A crash
// cannot publish a parent twice or unlock deletion before that exact input is
// represented. Intermediate levels receive fair service in each work slice.
func (t *Telemetry) PromoteSummariesChunk(ctx context.Context, now time.Time, limit int) (int, error) {
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
	through, err := t.progress(ctx, tx, "summary")
	if err != nil {
		return 0, err
	}
	done := 0
	for done < limit {
		progress := false
		for _, tier := range telemetryPromotionTiers {
			if done >= limit {
				break
			}
			var segment, series string
			var bucket, input int64
			q := "SELECT b.segment_id,b.series_id,b.bucket,a.last_id FROM " + t.table("summary") + " b JOIN " + t.table("segments") + " a ON a.id=b.segment_id WHERE b.resolution=? AND b.promoted_through=0 AND b.bucket<? AND a.summary_version=2 AND a.state NOT IN ('open','legacy') AND a.last_id<=? ORDER BY b.bucket,b.segment_id,b.series_id LIMIT 1"
			err = tx.QueryRowContext(ctx, t.bind(q), int64(tier.source), now.UTC().Truncate(tier.target).UnixNano(), through).Scan(&segment, &series, &bucket, &input)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return 0, err
			}
			from := time.Unix(0, bucket).UTC().Truncate(tier.target).UnixNano()
			end := from + int64(tier.target)
			// A target may not claim the whole range while any lower tier in
			// that range is still awaiting handoff. Otherwise the first minute
			// would publish a partial 15m parent and hide the other 14 minutes.
			var pending int
			err = tx.QueryRowContext(ctx, t.bind("SELECT 1 FROM "+t.table("summary")+" WHERE segment_id=? AND series_id=? AND resolution<? AND promoted_through=0 AND bucket>=? AND bucket<? LIMIT 1"), segment, series, int64(tier.source), from, end).Scan(&pending)
			if err == nil {
				continue
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return 0, err
			}
			rows, e := tx.QueryContext(ctx, t.bind("SELECT stats FROM "+t.table("summary")+" WHERE segment_id=? AND series_id=? AND resolution=? AND bucket>=? AND bucket<? ORDER BY bucket LIMIT 17"), segment, series, int64(tier.source), from, end)
			if e != nil {
				return 0, e
			}
			var stats telemetryStats
			n := 0
			for rows.Next() {
				var raw string
				var child telemetryStats
				n++
				if e = rows.Scan(&raw); e != nil {
					break
				}
				if e = json.Unmarshal([]byte(raw), &child); e != nil {
					break
				}
				if e = stats.merge(child); e != nil {
					break
				}
			}
			re := rows.Err()
			rows.Close()
			if e != nil {
				return 0, e
			}
			if re != nil {
				return 0, re
			}
			if n == 0 || n > int(tier.target/tier.source) {
				return 0, ErrTelemetryCoverage
			}
			encoded, e := stats.encodeSealed()
			if e != nil {
				return 0, e
			}
			// A v2 target is derived from this complete immutable child range.
			// Replacing it is idempotent; source cells cannot have been pruned
			// until the same transaction previously marked them as handed off.
			insert := "INSERT INTO " + t.table("summary") + " (segment_id,series_id,resolution,bucket,stats,input_through,promoted_through) VALUES (?,?,?,?,?,?,0)"
			if t.s.cfg.Driver == DriverMySQL {
				insert += " ON DUPLICATE KEY UPDATE stats=VALUES(stats),input_through=VALUES(input_through)"
			} else {
				insert += " ON CONFLICT(segment_id,series_id,resolution,bucket) DO UPDATE SET stats=excluded.stats,input_through=excluded.input_through"
			}
			if _, err = tx.ExecContext(ctx, t.bind(insert), segment, series, int64(tier.target), from, encoded, input); err != nil {
				return 0, err
			}
			if err = t.fault("tier-parent-before-handoff"); err != nil {
				return 0, err
			}
			if _, err = tx.ExecContext(ctx, t.bind("UPDATE "+t.table("summary")+" SET promoted_through=? WHERE segment_id=? AND series_id=? AND resolution=? AND bucket>=? AND bucket<?"), input, segment, series, int64(tier.source), from, end); err != nil {
				return 0, err
			}
			done++
			progress = true
		}
		if !progress {
			break
		}
	}
	if err = t.fault("tier-before-commit"); err != nil {
		return 0, err
	}
	return done, tx.Commit()
}
