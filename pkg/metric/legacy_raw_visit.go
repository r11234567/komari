package metric

import (
	"context"
	"errors"
	"time"
)

// VisitLegacyRaw is an explicitly offline, bounded bridge for business-ledger
// migration. It never substitutes a rollup average for a raw observation.
func (s *Store) VisitLegacyRaw(ctx context.Context, name, entity string, start, end time.Time, limit int, visit func([]Point) error) error {
	if s.Telemetry() != nil {
		return errors.New("raw legacy traversal requires offline migration")
	}
	if limit < 1 || limit > 131072 || end.Before(start) || end.Sub(start) > time.Hour {
		return ErrInvalidArgument
	}
	remaining := limit
	return s.visitLegacyMigrationRaw(ctx, &Telemetry{s: s}, name, entity, start, end, func(points []Point) error {
		remaining -= len(points)
		if remaining < 0 {
			return ErrTelemetryBackpressure
		}
		return visit(points)
	})
}
