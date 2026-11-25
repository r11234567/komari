package trafficledger

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/pkg/metric"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func updateTrafficCutoff(tx *gorm.DB, client, cycle string, at int64, delta Usage) error {
	if delta == (Usage{}) {
		return nil
	}
	return tx.Model(&models.TrafficCalibrationCutoff{}).
		Where("client = ? AND cycle = ? AND cutoff > ?", client, cycle, at).
		UpdateColumns(map[string]any{"raw_up": gorm.Expr("raw_up + ?", delta.Up), "raw_down": gorm.Expr("raw_down + ?", delta.Down)}).Error
}

func updateCutoffsAcrossLongInterval(tx *gorm.DB, client, cycle string, start, end time.Time, delta Usage) error {
	if cycle == "" || delta == (Usage{}) {
		return nil
	}
	var cutoffs []models.TrafficCalibrationCutoff
	if err := tx.Select("id", "cutoff").Where("client = ? AND cycle = ? AND cutoff > ? AND cutoff < ?", client, cycle, start.UnixNano(), end.UnixNano()).Find(&cutoffs).Error; err != nil {
		return err
	}
	for _, row := range cutoffs {
		part := longIntervalPrefix(delta, start.UnixNano(), end.UnixNano(), row.Cutoff)
		if err := tx.Model(&row).UpdateColumns(map[string]any{
			"raw_up": gorm.Expr("raw_up + ?", part.Up), "raw_down": gorm.Expr("raw_down + ?", part.Down),
		}).Error; err != nil {
			return err
		}
	}
	return nil
}

func longIntervalPrefix(delta Usage, start, end, cutoff int64) Usage {
	span, through := time.Duration(end-start), time.Duration(cutoff-start)
	return Usage{Up: trafficProportion(delta.Up, through, span), Down: trafficProportion(delta.Down, through, span)}
}

// A cutoff owns a minute boundary. Late traffic updates its raw prefix in the
// ledger transaction, independently of minute history retention.
func SaveTrafficCutoff(ctx context.Context, db *gorm.DB, client models.Client, cycle string, cutoff time.Time, target Usage, targetTotal *int64, countingType, operator, note string, now time.Time) (CalibrationSnapshot, error) {
	if cutoff.IsZero() || !cutoff.Equal(cutoff.Truncate(time.Minute)) || cutoff.After(now) || cutoff.Before(now.Add(-24*time.Hour)) {
		return CalibrationSnapshot{}, fmt.Errorf("calibration time must be a whole minute within the last 24 hours")
	}
	if target.Up < 0 || target.Down < 0 || (targetTotal != nil && *targetTotal < 0) {
		return CalibrationSnapshot{}, metric.ErrInvalidArgument
	}
	telemetryTrafficMu.Lock()
	defer telemetryTrafficMu.Unlock()
	var snapshot CalibrationSnapshot
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("uuid = ?", client.UUID).Take(&client).Error; err != nil {
			return err
		}
		var err error
		snapshot, err = loadCalibrationSnapshot(ctx, tx, client, now)
		if err != nil {
			return err
		}
		if cycle != snapshot.Cycle || cutoff.Before(snapshot.CycleStart) {
			return fmt.Errorf("calibration time must be in the current billing cycle")
		}
		var state models.TelemetryTrafficState
		if err = tx.Where("client = ?", client.UUID).Take(&state).Error; err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if cutoff.UnixNano() < state.MinutePrunedThrough || (state.MinuteSince > cutoff.UnixNano() && state.FirstObserved < state.MinuteSince) {
			return fmt.Errorf("%w: calibration time predates minute-resolution traffic", metric.ErrTelemetryCoverage)
		}
		after, err := trafficAfterCutoff(tx, client.UUID, cutoff, snapshot.CycleEnd.Add(time.Nanosecond), state.LastObserved)
		if err != nil {
			return err
		}
		if after.Up > snapshot.Raw.Up || after.Down > snapshot.Raw.Down {
			return fmt.Errorf("%w: retained traffic exceeds the current cycle", metric.ErrTelemetryCoverage)
		}
		rawBefore := Usage{Up: snapshot.Raw.Up - after.Up, Down: snapshot.Raw.Down - after.Down}
		if targetTotal != nil {
			if countingType != NormalizedCountingType(client.TrafficLimitType) {
				return fmt.Errorf("traffic counting method changed")
			}
			target, err = CalibrationTargetsForTotal(countingType, rawBefore, *targetTotal)
			if err != nil {
				return err
			}
		}
		id, err := newCalibrationID()
		if err != nil {
			return err
		}
		row := models.TrafficCalibrationCutoff{
			ID: id, Client: client.UUID, Cycle: cycle, Cutoff: cutoff.UnixNano(), RawUp: rawBefore.Up, RawDown: rawBefore.Down,
			BasisUp: rawBefore.Up, BasisDown: rawBefore.Down,
			TargetUp: target.Up, TargetDown: target.Down, TargetTotal: targetTotal, CountingType: countingType,
			Operator: operator, Note: note, CreatedAt: now.UTC(),
		}
		if err = tx.Create(&row).Error; err != nil {
			return err
		}
		snapshot, err = loadCalibrationSnapshot(ctx, tx, client, now)
		return err
	})
	if err == nil {
		InvalidateCalibratedCycleCache()
	}
	return snapshot, err
}

func trafficAfterCutoff(tx *gorm.DB, client string, cutoff, end time.Time, last int64) (Usage, error) {
	const sum = "COALESCE(SUM(up_bytes), 0) AS up, COALESCE(SUM(down_bytes), 0) AS down"
	var after Usage
	wholeEnd := end.Truncate(time.Minute)
	if err := tx.Model(&models.TelemetryTrafficMinute{}).Select(sum).
		Where("client = ? AND start >= ? AND start < ?", client, cutoff.UnixNano(), wholeEnd.UnixNano()).
		Scan(&after).Error; err != nil {
		return Usage{}, err
	}
	if !end.Equal(wholeEnd) && !wholeEnd.Before(cutoff) && last >= wholeEnd.UnixNano() {
		var edge Usage
		if err := tx.Model(&models.TelemetryTrafficTail{}).Select(sum).
			Where("client = ? AND observed >= ? AND observed < ?", client, wholeEnd.UnixNano(), end.UnixNano()).
			Scan(&edge).Error; err != nil {
			return Usage{}, err
		}
		after = addUsage(after, edge)
	}
	var long []models.TelemetryTrafficGap
	if err := tx.Where("client = ? AND kind = ? AND start < ? AND end > ? AND end <= ?", client, "long_interval", cutoff.UnixNano(), cutoff.UnixNano(), end.UnixNano()).Find(&long).Error; err != nil {
		return Usage{}, err
	}
	for _, gap := range long {
		part := longIntervalPrefix(Usage{Up: gap.UpBytes, Down: gap.DownBytes}, gap.Start, gap.End, cutoff.UnixNano())
		if part.Up > after.Up || part.Down > after.Down {
			return Usage{}, metric.ErrTelemetryCoverage
		}
		after.Up -= part.Up
		after.Down -= part.Down
	}
	return after, nil
}

func applyCutoffCalibration(ctx context.Context, db *gorm.DB, snapshot *CalibrationSnapshot, countingType string) (bool, error) {
	var rows []models.TrafficCalibrationCutoff
	if err := db.WithContext(ctx).Where("client = ? AND cycle = ?", snapshot.Client, snapshot.Cycle).
		Order("created_at DESC, id DESC").Limit(10).Find(&rows).Error; err != nil {
		return false, err
	}
	for _, row := range rows {
		mode := "directions"
		if row.TargetTotal != nil {
			mode = "total:" + row.CountingType
		}
		snapshot.History = append(snapshot.History, CalibrationHistory{
			CalibrationID: row.ID, CreatedAt: row.CreatedAt, Cutoff: time.Unix(0, row.Cutoff).UTC().Format(time.RFC3339), Mode: mode,
			TargetTotal: row.TargetTotal, Operator: row.Operator, Note: row.Note, Target: Usage{Up: row.TargetUp, Down: row.TargetDown},
			Adjustment: SignedUsage{Up: row.TargetUp - row.RawUp, Down: row.TargetDown - row.RawDown},
		})
	}
	sort.SliceStable(snapshot.History, func(i, j int) bool { return snapshot.History[i].CreatedAt.After(snapshot.History[j].CreatedAt) })
	snapshot.History = snapshot.History[:min(10, len(snapshot.History))]
	if len(rows) == 0 || snapshot.History[0].CalibrationID != rows[0].ID {
		return false, nil
	}
	row := rows[0]
	snapshot.HistoryComplete = true
	after := Usage{Up: max(0, snapshot.Raw.Up-row.RawUp), Down: max(0, snapshot.Raw.Down-row.RawDown)}
	effective := addUsage(Usage{Up: row.TargetUp, Down: row.TargetDown}, after)
	if row.TargetTotal != nil {
		if row.CountingType != countingType {
			snapshot.CalibrationWarning = "计费口径已变化，原总量校准已停用，请重新校准"
			snapshot.Effective, snapshot.Adjustment = snapshot.Raw, SignedUsage{}
			return true, nil
		}
		basisAndGrowth := addUsage(Usage{Up: row.BasisUp, Down: row.BasisDown}, after)
		withGrowth := BillableUsage(countingType, basisAndGrowth.Up, basisAndGrowth.Down)
		basisBillable := BillableUsage(countingType, row.BasisUp, row.BasisDown)
		billable := saturatingAdd(*row.TargetTotal, max(0, withGrowth-basisBillable))
		var err error
		effective, err = CalibrationTargetsForTotal(countingType, effective, billable)
		if err != nil {
			return false, err
		}
	}
	snapshot.Effective = effective
	snapshot.Adjustment = SignedUsage{Up: effective.Up - snapshot.Raw.Up, Down: effective.Down - snapshot.Raw.Down}
	return true, nil
}
