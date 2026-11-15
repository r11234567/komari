package trafficledger

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/komari-monitor/komari/database/metricstore"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/pkg/metric"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var telemetryMetadata atomic.Pointer[gorm.DB]
var telemetryTrafficMu sync.Mutex

// ConfigureTelemetry binds the existing metadata database before the server
// accepts reports. There is no dependency on raw metric retention or queries.
func ConfigureTelemetry(db *gorm.DB) { telemetryMetadata.Store(db) }

func TelemetryModels() []any {
	return []any{&models.TelemetryTrafficStream{}, &models.TelemetryTrafficState{}, &models.TelemetryTrafficSlice{}, &models.TelemetryTrafficCycle{}, &models.TelemetryTrafficMinute{}, &models.TelemetryTrafficTail{}}
}

func ConsumeTelemetry(ctx context.Context, event metric.TelemetryTrafficEvent) error {
	db := telemetryMetadata.Load()
	if db == nil {
		return errors.New("telemetry traffic metadata is not configured")
	}
	return ApplyTelemetry(ctx, db, event)
}

func ensureTelemetryFresh(ctx context.Context) error {
	// Accounting is owned exclusively by the persistent background consumer.
	// A read must never claim an outbox watermark, take a write lock, or make
	// its success depend on the number of queued reports. Read metadata exposes
	// the accounting observation time separately from the request time.
	return ctx.Err()
}

func telemetryUsage(ctx context.Context, clients []string, start, end time.Time, interval time.Duration) (map[string]Usage, map[string][]HourlyUsage, error) {
	if err := ensureTelemetryFresh(ctx); err != nil {
		return nil, nil, err
	}
	db := telemetryMetadata.Load()
	if db == nil {
		return nil, nil, errors.New("telemetry metadata unavailable")
	}
	return TelemetryUsageBatch(ctx, db, clients, start, end, interval)
}

// ApplyTelemetry is the idempotent metadata half of the cross-database outbox.
// A replay after metadata COMMIT but before metric outbox ACK changes nothing.
// The producer delivers a Store's pending outbox in ascending ingest order.
func ApplyTelemetry(ctx context.Context, db *gorm.DB, event metric.TelemetryTrafficEvent) error {
	if db == nil || event.StoreID == "" || event.IngestID <= 0 || event.Source == "" || event.Observed.IsZero() {
		return metric.ErrInvalidArgument
	}
	if event.Traffic.TotalUp < 0 || event.Traffic.TotalDown < 0 || event.Traffic.Up < 0 || event.Traffic.Down < 0 {
		return metric.ErrInvalidArgument
	}
	// SQLite has one metadata writer. CAS additionally protects other processes
	// and external metadata drivers; a failed claim rolls the entire delta back.
	telemetryTrafficMu.Lock()
	defer telemetryTrafficMu.Unlock()
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		initial := models.TelemetryTrafficStream{StoreID: event.StoreID}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&initial).Error; err != nil {
			return err
		}
		var stream models.TelemetryTrafficStream
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("store_id = ?", event.StoreID).Take(&stream).Error; err != nil {
			return err
		}
		if event.IngestID <= stream.Through {
			return nil
		}
		claim := tx.Model(&models.TelemetryTrafficStream{}).Where("store_id = ? AND through = ?", event.StoreID, stream.Through).Update("through", event.IngestID)
		if claim.Error != nil {
			return claim.Error
		}
		if claim.RowsAffected != 1 {
			return metric.ErrTelemetryBackpressure
		}
		var client models.Client
		if err := tx.Where("uuid = ?", event.Source).Take(&client).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		} else if err != nil {
			return err
		}
		var state models.TelemetryTrafficState
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("client = ?", event.Source).Take(&state).Error
		found := err == nil
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		ns := event.Observed.UnixNano()
		delta := Usage{}
		if found && ns <= state.LastObserved {
			// Late measurements must not rewind a counter epoch. Keep an explicit
			// diagnostic instead of manufacturing a second transfer interval.
			return tx.Model(&state).UpdateColumn("late_reports", gorm.Expr("late_reports + 1")).Error
		}
		if found {
			elapsed := event.Observed.Sub(time.Unix(0, state.LastObserved))
			delta.Up = metricstore.ReportTrafficCounterDelta(event.Traffic.TotalUp, state.TotalUp, event.Traffic.Up, elapsed)
			delta.Down = metricstore.ReportTrafficCounterDelta(event.Traffic.TotalDown, state.TotalDown, event.Traffic.Down, elapsed)
		} else {
			state = models.TelemetryTrafficState{Client: event.Source, FirstObserved: ns}
		}
		if event.Traffic.HasDelta {
			delta = Usage{Up: event.Traffic.DeltaUp, Down: event.Traffic.DeltaDown}
			if delta.Up < 0 || delta.Down < 0 {
				return metric.ErrInvalidArgument
			}
		}
		if state.MinuteSince == 0 {
			state.MinuteSince = ns
			if found {
				// All earlier accepted events are represented only by old hourly
				// rows. Do not advertise minute precision for that history.
				state.MinuteSince = state.LastObserved + 1
			}
		}
		state.LastObserved = ns
		state.TotalUp = event.Traffic.TotalUp
		state.TotalDown = event.Traffic.TotalDown
		state.AccountedUp = saturatingAdd(state.AccountedUp, delta.Up)
		state.AccountedDown = saturatingAdd(state.AccountedDown, delta.Down)
		if err = tx.Save(&state).Error; err != nil {
			return err
		}
		day := models.TrafficDailyLedger{Client: event.Source, Day: dayKey(event.Observed), UpBytes: delta.Up, DownBytes: delta.Down}
		if err = tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "client"}, {Name: "day"}}, DoUpdates: clause.Assignments(map[string]any{"up_bytes": gorm.Expr("up_bytes + ?", delta.Up), "down_bytes": gorm.Expr("down_bytes + ?", delta.Down), "updated_at": event.Received})}).Create(&day).Error; err != nil {
			return err
		}
		hourStart := event.Observed.Truncate(time.Hour)
		hourEnd := hourStart.Add(time.Hour)
		schedule := clientSchedule(client)
		if schedule.Active() {
			cycleStart := schedule.Last(event.Observed)
			cycleEnd := schedule.Next(cycleStart)
			if cycleStart.After(hourStart) {
				hourStart = cycleStart
			}
			if cycleEnd.Before(hourEnd) {
				hourEnd = cycleEnd
			}
			cycle := models.TelemetryTrafficCycle{Client: event.Source, Cycle: schedule.CycleKey(event.Observed), Start: cycleStart.UnixNano(), End: cycleEnd.UnixNano(), UpBytes: delta.Up, DownBytes: delta.Down, Through: ns}
			if err = tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "client"}, {Name: "cycle"}}, DoUpdates: clause.Assignments(map[string]any{"up_bytes": gorm.Expr("up_bytes + ?", delta.Up), "down_bytes": gorm.Expr("down_bytes + ?", delta.Down), "through": ns})}).Create(&cycle).Error; err != nil {
				return err
			}
			// Maintain the special first calendar day directly from events. The
			// calibration reader can then outlive both raw and hourly retention.
			if !isBeijingMidnight(cycleStart) && BeijingDay(cycleStart).Equal(BeijingDay(event.Observed)) {
				first := models.TrafficCycleFirstDay{Client: event.Source, Cycle: cycle.Cycle, Day: dayKey(event.Observed), UpBytes: delta.Up, DownBytes: delta.Down, CoveredUntil: event.Observed.Add(time.Nanosecond)}
				if err = tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "client"}, {Name: "cycle"}}, DoUpdates: clause.Assignments(map[string]any{"up_bytes": gorm.Expr("up_bytes + ?", delta.Up), "down_bytes": gorm.Expr("down_bytes + ?", delta.Down), "covered_until": first.CoveredUntil})}).Create(&first).Error; err != nil {
					return err
				}
			}
		}
		var slice models.TelemetryTrafficSlice
		err = tx.Where("client = ? AND start = ?", event.Source, hourStart.UnixNano()).Take(&slice).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			slice = models.TelemetryTrafficSlice{Client: event.Source, Start: hourStart.UnixNano(), End: hourEnd.UnixNano(), FirstObserved: ns}
		} else if err != nil {
			return err
		}
		// A settings change cannot poison the global outbox. Retain the actual
		// observed bounds; current-cycle totals use the separately keyed cycles.
		slice.End = max(slice.End, hourEnd.UnixNano())
		slice.LastObserved = ns
		slice.UpBytes = saturatingAdd(slice.UpBytes, delta.Up)
		slice.DownBytes = saturatingAdd(slice.DownBytes, delta.Down)
		if err = tx.Save(&slice).Error; err != nil {
			return err
		}
		return applyTelemetryMinute(tx, event.Source, event.Observed, delta)
	})
	if err == nil {
		InvalidateCalibratedCycleCache()
	}
	return err
}

// TelemetryUsageBatch reads only compact business ledgers. Partial historical
// slices whose exact attribution was not retained return an explicit coverage
// error; there is no hidden scan of raw metrics or archives.
func telemetryHourlyUsageBatch(ctx context.Context, db *gorm.DB, clients []string, start, end time.Time, interval time.Duration) (map[string]Usage, map[string][]HourlyUsage, error) {
	if db == nil || len(clients) > 256 || interval <= 0 || end.Before(start) {
		return nil, nil, metric.ErrInvalidArgument
	}
	totals := map[string]Usage{}
	hourly := map[string][]HourlyUsage{}
	for _, client := range clients {
		var state models.TelemetryTrafficState
		if err := db.WithContext(ctx).Where("client = ?", client).Take(&state).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			totals[client] = Usage{}
			continue
		} else if err != nil {
			return nil, nil, err
		}
		if start.UnixNano() < state.HourlyPrunedThrough && end.UnixNano() >= state.FirstObserved {
			return nil, nil, fmt.Errorf("%w: hourly traffic ledger has expired", metric.ErrTelemetryCoverage)
		}
		var slices []models.TelemetryTrafficSlice
		if err := db.WithContext(ctx).Where("client = ? AND start <= ? AND end > ?", client, end.UnixNano(), start.UnixNano()).Order("start").Limit(10001).Find(&slices).Error; err != nil {
			return nil, nil, err
		}
		if len(slices) > 10000 {
			return nil, nil, metric.ErrTelemetryBackpressure
		}
		buckets := map[int64]Usage{}
		for _, s := range slices {
			if s.LastObserved < start.UnixNano() || s.FirstObserved > end.UnixNano() {
				continue
			}
			if s.FirstObserved < start.UnixNano() || s.LastObserved > end.UnixNano() {
				return nil, nil, fmt.Errorf("%w: partial traffic ledger slice", metric.ErrTelemetryCoverage)
			}
			value := Usage{Up: s.UpBytes, Down: s.DownBytes}
			total := totals[client]
			total.Up = saturatingAdd(total.Up, value.Up)
			total.Down = saturatingAdd(total.Down, value.Down)
			totals[client] = total
			bucket := time.Unix(0, s.Start).In(BeijingLocation).Truncate(interval).UnixNano()
			v := buckets[bucket]
			v.Up = saturatingAdd(v.Up, value.Up)
			v.Down = saturatingAdd(v.Down, value.Down)
			buckets[bucket] = v
		}
		var times []int64
		for ns := range buckets {
			times = append(times, ns)
		}
		sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
		for _, ns := range times {
			hourly[client] = append(hourly[client], HourlyUsage{Hour: time.Unix(0, ns).In(BeijingLocation), Usage: buckets[ns]})
		}
	}
	return totals, hourly, nil
}
