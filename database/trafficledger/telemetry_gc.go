package trafficledger

import (
	"context"
	"sort"
	"time"

	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/pkg/metric"
	"gorm.io/gorm"
)

type trafficPruneRow struct {
	Client  string
	ItemKey int64
	Through int64
	kind    int
	due     int64
}

// PruneTelemetrySlices expires hour/minute/edge rows in one bounded, fair
// transaction. Watermarks and deletion commit together. Lifetime counters,
// daily ledgers, billing cycles and calibration history are independent.
func PruneTelemetrySlices(ctx context.Context, now time.Time, limit int) (int, error) {
	if limit < 1 || limit > 512 {
		return 0, metric.ErrInvalidArgument
	}
	db := telemetryMetadata.Load()
	if db == nil {
		return 0, nil
	}
	count := 0
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		specs := []struct {
			model                any
			key, through, filter string
			cutoff, retention    int64
			watermark            string
			order                string
		}{
			{&models.TelemetryTrafficSlice{}, "start", "end", "end < ?", now.AddDate(0, 0, -35).UnixNano(), int64(35 * 24 * time.Hour), "hourly_pruned_through", "end, client, start"},
			{&models.TelemetryTrafficMinute{}, "start", "start + 60000000000", "start < ?", now.Add(-telemetryMinuteRetention).Truncate(time.Minute).UnixNano(), int64(telemetryMinuteRetention), "minute_pruned_through", "start, client"},
			{&models.TelemetryTrafficTail{}, "observed", "observed + 1", "observed < ?", now.Add(-telemetryTailRetention).UnixNano(), int64(telemetryTailRetention), "tail_pruned_through", "observed, client"},
		}
		var candidates []trafficPruneRow
		for kind, spec := range specs {
			var rows []trafficPruneRow
			if err := tx.Model(spec.model).Select("client, "+spec.key+" AS item_key, "+spec.through+" AS through").Where(spec.filter, spec.cutoff).Order(spec.order).Limit(limit).Find(&rows).Error; err != nil {
				return err
			}
			for _, row := range rows {
				row.kind = kind
				row.due = row.Through + spec.retention
				candidates = append(candidates, row)
			}
		}
		sort.Slice(candidates, func(i, j int) bool {
			if candidates[i].due == candidates[j].due {
				if candidates[i].Client == candidates[j].Client {
					return candidates[i].kind < candidates[j].kind
				}
				return candidates[i].Client < candidates[j].Client
			}
			return candidates[i].due < candidates[j].due
		})
		candidates = candidates[:min(limit, len(candidates))]
		for kind, spec := range specs {
			keys := make([][]any, 0, len(candidates))
			watermarks := map[string]int64{}
			for _, row := range candidates {
				if row.kind == kind {
					keys = append(keys, []any{row.Client, row.ItemKey})
					watermarks[row.Client] = max(watermarks[row.Client], row.Through)
				}
			}
			clients := make([]string, 0, len(watermarks))
			for id := range watermarks {
				clients = append(clients, id)
			}
			sort.Strings(clients)
			for _, id := range clients {
				through := watermarks[id]
				if err := tx.Model(&models.TelemetryTrafficState{}).Where("client = ?", id).UpdateColumn(spec.watermark, gorm.Expr("CASE WHEN "+spec.watermark+" < ? THEN ? ELSE "+spec.watermark+" END", through, through)).Error; err != nil {
					return err
				}
			}
			if len(keys) > 0 {
				result := tx.Where("(client, "+spec.key+") IN ?", keys).Delete(spec.model)
				if result.Error != nil {
					return result.Error
				}
				count += int(result.RowsAffected)
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return count, nil
}
