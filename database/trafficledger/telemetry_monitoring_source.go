package trafficledger

import (
	"context"
	"time"

	"github.com/komari-monitor/komari/database/metricstore"
	"github.com/komari-monitor/komari/pkg/metric"
	"gorm.io/gorm"
)

// MigrateMonitoringLedger uses the same settlement and counter recovery as the
// metrics upgrade, but reads the original monitoring rows. No intermediate
// metric point table or historical ingress/outbox is required.
func MigrateMonitoringLedger(ctx context.Context, db *gorm.DB, t *metric.Telemetry, now time.Time,
	read func(context.Context, string, time.Time, time.Time) ([]DeltaRecord, *DeltaRecord, error),
) error {
	return migrateTelemetryLedger(ctx, db, now, offlineTrafficSource{
		usage: func(ctx context.Context, client string, from, until time.Time) (Usage, error) {
			rows, previous, err := read(ctx, client, from, until)
			if err != nil {
				return Usage{}, err
			}
			up, down := SumTrafficDeltas(rows, previous)
			return Usage{Up: up, Down: down}, nil
		},
		deltas: func(ctx context.Context, client string, from, until time.Time) ([]metricstore.LegacyTrafficDelta, bool, error) {
			rows, _, err := read(ctx, client, from, until)
			if err != nil {
				return nil, false, err
			}
			events := make([]metricstore.LegacyTrafficDelta, 0, len(rows))
			for _, row := range rows {
				if !row.TrafficUpSet || !row.TrafficDownSet || row.TrafficUp < 0 || row.TrafficDown < 0 {
					return nil, false, nil
				}
				events = append(events, metricstore.LegacyTrafficDelta{Observed: row.Time, Up: row.TrafficUp, Down: row.TrafficDown})
			}
			return events, len(events) > 0, nil
		},
		baseline: func(ctx context.Context, client string, before time.Time) (time.Time, int64, int64, bool, error) {
			rows, last, err := read(ctx, client, before, before)
			if err != nil {
				return time.Time{}, 0, 0, false, err
			}
			if len(rows) > 0 {
				last = &rows[len(rows)-1]
			}
			if last == nil {
				return time.Time{}, 0, 0, false, nil
			}
			return last.Time, last.NetTotalUp, last.NetTotalDown, true, nil
		},
		seed: t.SeedTrafficBaseline,
	})
}
