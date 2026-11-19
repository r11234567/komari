package metric

import (
	"context"
	"time"
)

// SeedTrafficBaseline is for the stopped-service migration only. An existing
// baseline is never rewound, including after an interrupted offline migration.
func (t *Telemetry) SeedTrafficBaseline(ctx context.Context, source string, at time.Time, up, down int64) error {
	if source == "" || at.IsZero() || up < 0 || down < 0 {
		return ErrInvalidArgument
	}
	q := "INSERT INTO " + t.table("traffic_ingress") + " (source,observed,total_up,total_down) VALUES (?,?,?,?)"
	if t.s.cfg.Driver == DriverMySQL {
		q += " ON DUPLICATE KEY UPDATE source=source"
	} else {
		q += " ON CONFLICT(source) DO NOTHING"
	}
	_, err := t.s.db.ExecContext(ctx, t.bind(q), source, at.UnixNano(), up, down)
	return err
}
