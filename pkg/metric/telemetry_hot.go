package metric

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

func (t *Telemetry) ReadHot(ctx context.Context, q Query, now time.Time) ([]Point, error) {
	if err := q.Validate(); err != nil {
		return nil, err
	}
	if q.Start.Before(now.Add(-t.cfg.RawWindow)) {
		return nil, ErrTelemetryCold
	}
	if q.Limit == 0 {
		q.Limit = 10000
	}
	if q.Limit > 10000 || q.Offset > 10000 {
		return nil, ErrTelemetryBackpressure
	}
	where := "s.metric=? AND h.observed>=? AND h.observed<=?"
	args := []any{q.MetricName, q.Start.UnixNano(), q.End.UnixNano()}
	if q.EntityID != "" {
		where += " AND s.entity=?"
		args = append(args, q.EntityID)
	}
	for k, v := range q.Tags {
		// Use the same restrictive tag-key grammar as the legacy SQL renderer.
		if k == "" || strings.IndexFunc(k, func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.')
		}) >= 0 {
			return nil, ErrInvalidArgument
		}
		column := "s.tags"
		if t.s.cfg.Driver == DriverPostgreSQL {
			column = "s.tags::jsonb"
		}
		where += " AND " + t.s.dialect.jsonExtractEquals(column, k, "?")
		args = append(args, v)
	}
	order := "ASC"
	if q.Order == OrderDesc {
		order = "DESC"
	}
	sqlText := fmt.Sprintf("SELECT s.metric,s.entity,s.tags,h.observed,h.value,h.labels FROM %s h JOIN %s s ON s.id=h.series_id WHERE %s ORDER BY h.observed %s,h.ingest_id %s,h.ordinal %s LIMIT ? OFFSET ?", t.table("hot"), t.table("series"), where, order, order, order)
	args = append(args, q.Limit, q.Offset)
	rows, err := t.s.reader().QueryContext(ctx, t.bind(sqlText), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Point, 0)
	for rows.Next() {
		var p Point
		var ns int64
		var tags, labels string
		if err = rows.Scan(&p.MetricName, &p.EntityID, &tags, &ns, &p.Value, &labels); err != nil {
			return nil, err
		}
		p.Timestamp = time.Unix(0, ns).UTC()
		if err = json.Unmarshal([]byte(tags), &p.Tags); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(labels), &p.Labels); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
