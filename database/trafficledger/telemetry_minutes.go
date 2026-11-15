package trafficledger

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/pkg/metric"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	telemetryMinuteRetention = 48 * time.Hour
	telemetryTailRetention   = 5 * time.Minute
	telemetryClientBatch     = 128
	telemetryMaxClients      = 4096
	telemetryMaxResultGroups = 100000
)

func applyTelemetryMinute(tx *gorm.DB, client string, observed time.Time, delta Usage) error {
	ns := observed.UnixNano()
	row := models.TelemetryTrafficMinute{Client: client, Start: observed.Truncate(time.Minute).UnixNano(), FirstObserved: ns, LastObserved: ns, UpBytes: delta.Up, DownBytes: delta.Down}
	if err := tx.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "client"}, {Name: "start"}},
		DoUpdates: clause.Assignments(map[string]any{
			"last_observed": ns,
			"up_bytes":      gorm.Expr("up_bytes + ?", delta.Up),
			"down_bytes":    gorm.Expr("down_bytes + ?", delta.Down),
		}),
	}).Create(&row).Error; err != nil {
		return err
	}
	return tx.Create(&models.TelemetryTrafficTail{Client: client, Observed: ns, UpBytes: delta.Up, DownBytes: delta.Down}).Error
}

// TelemetryUsageBatch keeps the legacy inclusive endpoint adapter. All internal
// whole-minute selection and output buckets use half-open intervals. No request
// drains the cross-database outbox, scans metric raw or decodes cold archives.
func TelemetryUsageBatch(ctx context.Context, db *gorm.DB, clients []string, start, end time.Time, interval time.Duration) (map[string]Usage, map[string][]HourlyUsage, error) {
	return telemetryUsageBatch(ctx, db, clients, start, end, interval, nil)
}

// AccountingThrough is an observation watermark, not request generation time.
// Offline clients can have older watermarks; outbox backlog is separate.
type TelemetryReadInfo struct {
	AccountingThrough time.Time
	MissingClients    int
}

func TelemetryUsageSnapshot(ctx context.Context, clients []string, start, end time.Time, interval time.Duration) (map[string]Usage, map[string][]HourlyUsage, TelemetryReadInfo, error) {
	var info TelemetryReadInfo
	db := telemetryMetadata.Load()
	if db == nil {
		return nil, nil, info, fmt.Errorf("telemetry metadata unavailable")
	}
	totals, groups, err := telemetryUsageBatch(ctx, db, clients, start, end, interval, &info)
	return totals, groups, info, err
}

func telemetryUsageBatch(ctx context.Context, db *gorm.DB, clients []string, start, end time.Time, interval time.Duration, info *TelemetryReadInfo) (map[string]Usage, map[string][]HourlyUsage, error) {
	if db == nil || len(clients) > telemetryMaxClients || interval < time.Minute || interval%time.Minute != 0 || end.Before(start) || end.UnixNano() == math.MaxInt64 {
		return nil, nil, metric.ErrInvalidArgument
	}
	totals := make(map[string]Usage, len(clients))
	groups := make(map[string]map[int64]Usage, len(clients))
	result := make(map[string][]HourlyUsage, len(clients))
	unique := make([]string, 0, len(clients))
	for _, client := range clients {
		if client == "" {
			continue
		}
		if _, exists := totals[client]; !exists {
			totals[client] = Usage{}
			unique = append(unique, client)
		}
	}
	groupCount := 0
	emit := func(client string, bucket int64, value Usage) error {
		if groups[client] == nil {
			groups[client] = make(map[int64]Usage)
		}
		old, exists := groups[client][bucket]
		if !exists {
			groupCount++
			if groupCount > telemetryMaxResultGroups {
				return metric.ErrTelemetryBackpressure
			}
		}
		groups[client][bucket] = addUsageSaturated(old, value)
		totals[client] = addUsageSaturated(totals[client], value)
		return nil
	}
	err := WithTelemetryReadSnapshot(ctx, db, func(tx *gorm.DB) error {
		for offset := 0; offset < len(unique); offset += telemetryClientBatch {
			ids := unique[offset:min(offset+telemetryClientBatch, len(unique))]
			var states []models.TelemetryTrafficState
			if err := tx.Where("client IN ?", ids).Find(&states).Error; err != nil {
				return err
			}
			fine := make([]string, 0, len(states))
			if info != nil {
				info.MissingClients += len(ids) - len(states)
			}
			byClient := make(map[string]models.TelemetryTrafficState, len(states))
			for _, state := range states {
				if info != nil && (info.AccountingThrough.IsZero() || state.LastObserved < info.AccountingThrough.UnixNano()) {
					info.AccountingThrough = time.Unix(0, min(state.LastObserved, end.UnixNano())).UTC()
				}
				byClient[state.Client] = state
				if state.MinuteSince > 0 && (start.UnixNano() >= max(state.MinuteSince, state.MinutePrunedThrough) || state.MinuteSince == state.FirstObserved && state.MinutePrunedThrough <= state.FirstObserved) {
					fine = append(fine, state.Client)
					continue
				}
				if interval%time.Hour != 0 {
					return fmt.Errorf("%w: minute traffic history is unavailable", metric.ErrTelemetryCoverage)
				}
				oldTotals, oldGroups, err := telemetryHourlyUsageBatch(ctx, tx, []string{state.Client}, start, end, interval)
				if err != nil {
					return err
				}
				totals[state.Client] = oldTotals[state.Client]
				result[state.Client] = oldGroups[state.Client]
			}
			if err := readTelemetryMinutes(tx, fine, byClient, start, end, interval, emit); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	for client, buckets := range groups {
		keys := make([]int64, 0, len(buckets))
		for key := range buckets {
			keys = append(keys, key)
		}
		sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
		for _, key := range keys {
			result[client] = append(result[client], HourlyUsage{Hour: time.Unix(0, key).In(BeijingLocation), Usage: buckets[key]})
		}
	}
	return totals, result, nil
}

func addUsageSaturated(a, b Usage) Usage {
	return Usage{Up: saturatingAdd(a.Up, b.Up), Down: saturatingAdd(a.Down, b.Down)}
}

func readTelemetryMinutes(tx *gorm.DB, clients []string, states map[string]models.TelemetryTrafficState, start, end time.Time, interval time.Duration, emit func(string, int64, Usage) error) error {
	if len(clients) == 0 {
		return nil
	}
	endExclusive := end.Add(time.Nanosecond)
	wholeStart := start.Truncate(time.Minute)
	if wholeStart.Before(start) {
		wholeStart = wholeStart.Add(time.Minute)
	}
	wholeEnd := endExclusive.Truncate(time.Minute)
	origin := start.UTC().Truncate(interval).UnixNano()
	if wholeStart.Before(wholeEnd) {
		expression := "CAST((start - ?) / ? AS BIGINT) * ? + ?"
		if tx.Dialector.Name() == "mysql" {
			expression = "((start - ?) DIV ?) * ? + ?"
		}
		var sums []struct {
			Client    string
			Bucket    int64
			UpBytes   int64
			DownBytes int64
		}
		if err := tx.Model(&models.TelemetryTrafficMinute{}).
			Select("client, "+expression+" AS bucket, SUM(up_bytes) AS up_bytes, SUM(down_bytes) AS down_bytes", origin, int64(interval), int64(interval), origin).
			Where("client IN ? AND start >= ? AND start < ?", clients, wholeStart.UnixNano(), wholeEnd.UnixNano()).
			Group("client, bucket").Limit(telemetryMaxResultGroups + 1).Find(&sums).Error; err != nil {
			return err
		}
		if len(sums) > telemetryMaxResultGroups {
			return metric.ErrTelemetryBackpressure
		}
		for _, row := range sums {
			if err := emit(row.Client, row.Bucket, Usage{Up: row.UpBytes, Down: row.DownBytes}); err != nil {
				return err
			}
		}
	}
	// Only two partial minute boundaries can remain. Full minutes are never
	// reconstructed from tail records or counted for a second time.
	var boundaries []models.TelemetryTrafficMinute
	if err := tx.Where("client IN ? AND start >= ? AND start <= ? AND (start < ? OR start >= ?)", clients, start.Truncate(time.Minute).UnixNano(), end.Truncate(time.Minute).UnixNano(), wholeStart.UnixNano(), wholeEnd.UnixNano()).Find(&boundaries).Error; err != nil {
		return err
	}
	partial := make(map[string]map[int64]bool)
	for _, row := range boundaries {
		if row.LastObserved < start.UnixNano() || row.FirstObserved > end.UnixNano() {
			continue
		}
		if row.FirstObserved >= start.UnixNano() && row.LastObserved <= end.UnixNano() {
			if err := emit(row.Client, time.Unix(0, row.Start).UTC().Truncate(interval).UnixNano(), Usage{Up: row.UpBytes, Down: row.DownBytes}); err != nil {
				return err
			}
			continue
		}
		if max(row.FirstObserved, start.UnixNano()) < states[row.Client].TailPrunedThrough {
			return fmt.Errorf("%w: partial minute predates retained traffic edge", metric.ErrTelemetryCoverage)
		}
		if partial[row.Client] == nil {
			partial[row.Client] = make(map[int64]bool)
		}
		partial[row.Client][row.Start] = true
	}
	if len(partial) == 0 {
		return nil
	}
	ids := make([]string, 0, len(partial))
	for client := range partial {
		ids = append(ids, client)
	}
	var tail []models.TelemetryTrafficTail
	if err := tx.Where("client IN ? AND observed >= ? AND observed <= ? AND (observed < ? OR observed >= ?)", ids, start.UnixNano(), end.UnixNano(), wholeStart.UnixNano(), wholeEnd.UnixNano()).Order("client, observed").Limit(telemetryMaxResultGroups + 1).Find(&tail).Error; err != nil {
		return err
	}
	if len(tail) > telemetryMaxResultGroups {
		return metric.ErrTelemetryBackpressure
	}
	for _, row := range tail {
		at := time.Unix(0, row.Observed).UTC()
		if partial[row.Client][at.Truncate(time.Minute).UnixNano()] {
			if err := emit(row.Client, at.Truncate(interval).UnixNano(), Usage{Up: row.UpBytes, Down: row.DownBytes}); err != nil {
				return err
			}
		}
	}
	return nil
}
