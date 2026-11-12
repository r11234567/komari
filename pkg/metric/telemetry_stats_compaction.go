package metric

import (
	"context"
	"encoding/json"
	"strings"
)

type telemetryStatsCursor struct {
	Segment    string `json:"s"`
	Series     string `json:"i"`
	Resolution int64  `json:"r"`
	Bucket     int64  `json:"b"`
}

// CompactStatsChunk converts a bounded PK-ordered page of immutable cells.
// The durable scheduler owns the cursor. Opening a database never scans or
// recompresses history; cancellation rolls back the entire page. A repeated
// page is harmless and already compact cells are never recompressed.
func (t *Telemetry) CompactStatsChunk(ctx context.Context, cursor string, limit int) (int, string, error) {
	if limit < 1 || limit > 512 {
		return 0, cursor, ErrInvalidArgument
	}
	var after telemetryStatsCursor
	if cursor != "" {
		if err := json.Unmarshal([]byte(cursor), &after); err != nil {
			return 0, cursor, err
		}
	}
	release, err := t.acquire(ctx)
	if err != nil {
		return 0, cursor, err
	}
	defer release()
	tx, err := t.begin(ctx)
	if err != nil {
		return 0, cursor, err
	}
	defer tx.Rollback()
	through, err := t.progress(ctx, tx, "summary")
	if err != nil {
		return 0, cursor, err
	}
	query := "SELECT b.segment_id,b.series_id,b.resolution,b.bucket,b.stats FROM " + t.table("summary") + " b JOIN " + t.table("segments") + " a ON a.id=b.segment_id WHERE (b.segment_id,b.series_id,b.resolution,b.bucket)>(?,?,?,?) AND a.state<>'open' AND a.last_id<=? ORDER BY b.segment_id,b.series_id,b.resolution,b.bucket LIMIT ?"
	rows, err := tx.QueryContext(ctx, t.bind(query), after.Segment, after.Series, after.Resolution, after.Bucket, through, limit)
	if err != nil {
		return 0, cursor, err
	}
	type cell struct {
		key telemetryStatsCursor
		raw string
	}
	var cells []cell
	for rows.Next() {
		var c cell
		if err = rows.Scan(&c.key.Segment, &c.key.Series, &c.key.Resolution, &c.key.Bucket, &c.raw); err != nil {
			break
		}
		cells = append(cells, c)
	}
	re := rows.Err()
	rows.Close()
	if err != nil {
		return 0, cursor, err
	}
	if re != nil {
		return 0, cursor, re
	}
	for _, c := range cells {
		if strings.HasPrefix(c.raw, `"`+telemetryStatsPrefix) {
			continue
		}
		if err = telemetryYield(ctx); err != nil {
			return 0, cursor, err
		}
		var stats telemetryStats
		if err = json.Unmarshal([]byte(c.raw), &stats); err != nil {
			return 0, cursor, err
		}
		encoded, e := stats.encodeSealed()
		if e != nil {
			return 0, cursor, e
		}
		if encoded == c.raw {
			continue
		}
		k := c.key
		if _, err = tx.ExecContext(ctx, t.bind("UPDATE "+t.table("summary")+" SET stats=? WHERE segment_id=? AND series_id=? AND resolution=? AND bucket=?"), encoded, k.Segment, k.Series, k.Resolution, k.Bucket); err != nil {
			return 0, cursor, err
		}
	}
	if err = t.fault("stats-before-compaction-commit"); err != nil {
		return 0, cursor, err
	}
	if err = tx.Commit(); err != nil {
		return 0, cursor, err
	}
	if len(cells) == 0 {
		return 0, "", nil
	}
	b, err := json.Marshal(cells[len(cells)-1].key)
	return len(cells), string(b), err
}
