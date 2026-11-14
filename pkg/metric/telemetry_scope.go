package metric

import (
	"context"
	"strings"
)

type telemetryScopeKey struct{}

// WithTelemetryEntities restricts physical SQL and archive selection to an
// already authorized node set. An empty set permits no data. This belongs at
// the authenticated adapter, before a wildcard multi-series query is issued.
func WithTelemetryEntities(ctx context.Context, ids []string) context.Context {
	return context.WithValue(ctx, telemetryScopeKey{}, append([]string{}, ids...))
}

func (t *Telemetry) seriesFilter(ctx context.Context, q Query, alias string) (string, []any, error) {
	var where string
	var args []any
	if q.EntityID != "" {
		where += " AND " + alias + ".entity=?"
		args = append(args, q.EntityID)
	}
	if ids, ok := ctx.Value(telemetryScopeKey{}).([]string); ok {
		if len(ids) > 256 {
			return "", nil, ErrTelemetryBackpressure
		}
		if len(ids) == 0 {
			where += " AND 1=0"
		} else {
			where += " AND " + alias + ".entity IN (" + strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",") + ")"
			for _, id := range ids {
				args = append(args, id)
			}
		}
	}
	for _, k := range sortedKeys(q.Tags) {
		if k == "" || strings.IndexFunc(k, func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.')
		}) >= 0 {
			return "", nil, ErrInvalidArgument
		}
		column := alias + ".tags"
		if t.s.cfg.Driver == DriverPostgreSQL {
			column += "::jsonb"
		}
		where += " AND " + t.s.dialect.jsonExtractEquals(column, k, "?")
		args = append(args, q.Tags[k])
	}
	return where, args, nil
}
