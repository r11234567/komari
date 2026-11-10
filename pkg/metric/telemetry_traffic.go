package metric

import (
	"context"
	"database/sql"
	"errors"
	"github.com/komari-monitor/komari/pkg/trafficcounter"
	"time"
)

// deriveTraffic maintains only a small durable ingress baseline, not a query
// history. Deltas, complete raw report, receipt and outbox commit atomically.
// Business totals remain in the independent metadata ledger after handoff.
func (t *Telemetry) deriveTraffic(ctx context.Context, tx *sql.Tx, b *TelemetryBatch, observed int64) error {
	var prior, up, down int64
	err := tx.QueryRowContext(ctx, t.bind("SELECT observed,total_up,total_down FROM "+t.table("traffic_ingress")+" WHERE source=?"), b.Source).Scan(&prior, &up, &down)
	found := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	v := b.Traffic
	v.HasDelta = true
	v.DeltaUp, v.DeltaDown = 0, 0
	if v.ObservedAt != 0 {
		observed = v.ObservedAt
	}
	if found && observed > prior {
		elapsed := time.Duration(observed - prior)
		v.DeltaUp = trafficcounter.ReportDelta(v.TotalUp, up, v.Up, elapsed)
		v.DeltaDown = trafficcounter.ReportDelta(v.TotalDown, down, v.Down, elapsed)
	}
	if !found || observed > prior {
		q := "INSERT INTO " + t.table("traffic_ingress") + " (source,observed,total_up,total_down) VALUES (?,?,?,?)"
		if t.s.cfg.Driver == DriverMySQL {
			q += " ON DUPLICATE KEY UPDATE observed=VALUES(observed),total_up=VALUES(total_up),total_down=VALUES(total_down)"
		} else {
			q += " ON CONFLICT(source) DO UPDATE SET observed=excluded.observed,total_up=excluded.total_up,total_down=excluded.total_down"
		}
		if _, err = tx.ExecContext(ctx, t.bind(q), b.Source, observed, v.TotalUp, v.TotalDown); err != nil {
			return err
		}
	}
	for i, p := range b.Points {
		if p.MetricName == "traffic.up" {
			b.Points[i].Value = float64(v.DeltaUp)
		} else if p.MetricName == "traffic.down" {
			b.Points[i].Value = float64(v.DeltaDown)
		}
	}
	return nil
}
