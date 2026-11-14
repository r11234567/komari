package metric

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
)

type telemetryGapRange struct{ First, Last int64 }
type telemetryGapCursor struct {
	Segment string `json:"s"`
	Series  string `json:"i"`
	First   int64  `json:"f"`
}

// Packs exact disjoint intervals, never their outer envelope. A valid raw
// interval between missing ranges must remain exportable after compaction.
func encodeTelemetryGaps(ranges []telemetryGapRange) string {
	b := make([]byte, 0, len(ranges)*12)
	previous := int64(0)
	for _, r := range ranges {
		b = binary.AppendVarint(b, r.First-previous)
		b = binary.AppendVarint(b, r.Last-r.First)
		previous = r.First
	}
	return "TG1:" + base64.StdEncoding.EncodeToString(b)
}
func decodeTelemetryGaps(first, last int64, packed string) ([]telemetryGapRange, error) {
	if first > last {
		return nil, errors.New("invalid telemetry gap bounds")
	}
	if packed == "" {
		return []telemetryGapRange{{first, last}}, nil
	}
	if len(packed) < 4 || packed[:4] != "TG1:" || len(packed) > 14000 {
		return nil, errors.New("invalid telemetry gap encoding")
	}
	b, err := base64.StdEncoding.DecodeString(packed[4:])
	if err != nil {
		return nil, err
	}
	var result []telemetryGapRange
	previous := int64(0)
	for len(b) > 0 {
		delta, n := binary.Varint(b)
		if n <= 0 {
			return nil, errors.New("invalid telemetry gap start")
		}
		b = b[n:]
		width, n := binary.Varint(b)
		if n <= 0 || width < 0 {
			return nil, errors.New("invalid telemetry gap end")
		}
		b = b[n:]
		start := previous + delta
		end := start + width
		if start < first || end < start || end > last || (len(result) > 0 && start < previous) || len(result) >= 512 {
			return nil, errors.New("invalid telemetry gap bounds")
		}
		result = append(result, telemetryGapRange{start, end})
		previous = start
	}
	if len(result) == 0 || result[0].First != first {
		return nil, errors.New("missing telemetry gap ranges")
	}
	maxEnd := first
	for _, r := range result {
		maxEnd = max(maxEnd, r.Last)
	}
	if maxEnd != last {
		return nil, errors.New("incomplete telemetry gap envelope")
	}
	return result, nil
}

// This keyset pass removes repeated owner/series keys and indexes in bounded,
// atomic pages. Existing one-interval rows stay readable during lazy upgrade.
func (t *Telemetry) CompactGapsChunk(ctx context.Context, cursor string, limit int) (int, string, error) {
	if limit < 1 || limit > 512 {
		return 0, cursor, ErrInvalidArgument
	}
	var after telemetryGapCursor
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
	rows, err := tx.QueryContext(ctx, t.bind("SELECT segment_id,series_id,first_observed,last_observed FROM "+t.table("gaps")+" WHERE (segment_id,series_id,first_observed)>(?,?,?) AND ranges='' ORDER BY segment_id,series_id,first_observed LIMIT ?"), after.Segment, after.Series, after.First, limit)
	if err != nil {
		return 0, cursor, err
	}
	type cell struct {
		key  telemetryGapCursor
		last int64
	}
	var cells []cell
	for rows.Next() {
		var c cell
		if err = rows.Scan(&c.key.Segment, &c.key.Series, &c.key.First, &c.last); err != nil {
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
	for i := 0; i < len(cells); {
		if err = telemetryYield(ctx); err != nil {
			return 0, cursor, err
		}
		j := i + 1
		k := cells[i].key
		for j < len(cells) && cells[j].key.Segment == k.Segment && cells[j].key.Series == k.Series {
			j++
		}
		if j-i > 1 {
			var ranges []telemetryGapRange
			last := cells[i].last
			for _, c := range cells[i:j] {
				ranges = append(ranges, telemetryGapRange{c.key.First, c.last})
				last = max(last, c.last)
			}
			packed := encodeTelemetryGaps(ranges)
			if _, err = decodeTelemetryGaps(k.First, last, packed); err != nil {
				return 0, cursor, err
			}
			if _, err = tx.ExecContext(ctx, t.bind("UPDATE "+t.table("gaps")+" SET ranges=?,last_observed=? WHERE segment_id=? AND series_id=? AND first_observed=?"), packed, last, k.Segment, k.Series, k.First); err != nil {
				return 0, cursor, err
			}
			for _, c := range cells[i+1 : j] {
				if _, err = tx.ExecContext(ctx, t.bind("DELETE FROM "+t.table("gaps")+" WHERE segment_id=? AND series_id=? AND first_observed=?"), k.Segment, k.Series, c.key.First); err != nil {
					return 0, cursor, err
				}
			}
		}
		i = j
	}
	if err = t.fault("gaps-before-compaction-commit"); err != nil {
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

// A packed row's SQL bounds are an index prefilter, not proof of a missing
// observation. Inspect every candidate's exact ranges before rejecting raw.
func (t *Telemetry) exportHasLegacyGap(ctx context.Context, db querier, where string, args []any, start, end int64) (bool, error) {
	after := telemetryGapCursor{}
	for {
		params := append(append([]any(nil), args...), after.Segment, after.Series, after.First)
		query := "SELECT g.segment_id,g.series_id,g.first_observed,g.last_observed,g.ranges FROM " + t.table("gaps") + " g JOIN " + t.table("series") + " s ON s.id=g.series_id JOIN " + t.table("segments") + " a ON a.id=g.segment_id LEFT JOIN " + t.table("erasures") + " e ON e.series_id=s.id WHERE " + where + " AND (g.segment_id,g.series_id,g.first_observed)>(?,?,?) ORDER BY g.segment_id,g.series_id,g.first_observed LIMIT 32"
		rows, err := db.QueryContext(ctx, t.bind(query), params...)
		if err != nil {
			return false, err
		}
		n := 0
		found := false
		for rows.Next() {
			var last int64
			var packed string
			if err = rows.Scan(&after.Segment, &after.Series, &after.First, &last, &packed); err != nil {
				break
			}
			var ranges []telemetryGapRange
			ranges, err = decodeTelemetryGaps(after.First, last, packed)
			if err != nil {
				break
			}
			for _, r := range ranges {
				if r.First <= end && r.Last >= start {
					found = true
					break
				}
			}
			n++
			if found {
				break
			}
		}
		re := rows.Err()
		rows.Close()
		if err != nil {
			return false, err
		}
		if re != nil {
			return false, re
		}
		if found {
			return true, nil
		}
		if n < 32 {
			return false, nil
		}
		if err = telemetryYield(ctx); err != nil {
			return false, err
		}
	}
}
