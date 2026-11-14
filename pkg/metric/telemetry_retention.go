package metric

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// PruneSummaries removes only cells whose covering coarser deltas were committed
// with the same input cursor. This is independent of raw/archive retention.
// The scheduler supplies one metric and a bounded chunk, never a full DB sweep.
func (t *Telemetry) PruneSummaries(ctx context.Context, name string, retention time.Duration, now time.Time, limit int) (int, error) {
	if t.scalar {
		return t.pruneScalar(ctx, name, retention, now, limit)
	}
	if name == "" || retention <= 0 || limit <= 0 || limit > 512 {
		return 0, ErrInvalidArgument
	}
	release, err := t.acquire(ctx)
	if err != nil {
		return 0, err
	}
	defer release()
	tx, err := t.begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	where := "s.metric=? AND a.state<>'legacy' AND ("
	args := []any{name}
	tiers := append([]struct{ resolution, keep time.Duration }(nil), telemetrySummaryTiers[:]...)
	tiers = append(tiers, struct{ resolution, keep time.Duration }{15 * time.Minute, 2 * 24 * time.Hour})
	for i, tier := range tiers {
		if i > 0 {
			where += " OR "
		}
		where += "(b.resolution=? AND b.bucket<=? AND ((a.summary_version=1 AND b.resolution<?) OR b.promoted_through>=a.last_id OR b.bucket+b.resolution<=?))"
		keep := tier.keep
		if keep == 0 {
			keep = retention
		}
		args = append(args, int64(tier.resolution), now.Add(-min(keep, retention)).Add(-tier.resolution).UnixNano())
		args = append(args, int64(15*time.Minute))
		args = append(args, now.Add(-retention).UnixNano())
	}
	where += ") OR (s.metric=? AND a.state='legacy' AND b.bucket+b.resolution<=?)"
	args = append(args, name, now.Add(-retention).UnixNano())
	args = append(args, limit)
	rows, err := tx.QueryContext(ctx, t.bind("SELECT b.segment_id,b.series_id,b.resolution,b.bucket FROM "+t.table("summary")+" b JOIN "+t.table("series")+" s ON s.id=b.series_id JOIN "+t.table("segments")+" a ON a.id=b.segment_id WHERE "+where+" ORDER BY b.resolution,b.bucket,b.series_id,b.segment_id LIMIT ?"), args...)
	if err != nil {
		return 0, err
	}
	var keys []telemetrySummaryKey
	for rows.Next() {
		var k telemetrySummaryKey
		if err = rows.Scan(&k.segment, &k.series, &k.resolution, &k.bucket); err != nil {
			break
		}
		keys = append(keys, k)
	}
	readErr := rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	if readErr != nil {
		return 0, readErr
	}
	for _, k := range keys {
		if _, err = tx.ExecContext(ctx, t.bind("DELETE FROM "+t.table("summary")+" WHERE segment_id=? AND series_id=? AND resolution=? AND bucket=?"), k.segment, k.series, k.resolution, k.bucket); err != nil {
			return 0, err
		}
	}
	return len(keys), tx.Commit()
}

type TelemetryDiagnostics struct {
	Accepted             int64                      `json:"accepted_through"`
	Summarized           int64                      `json:"summarized_through"`
	HotBudgetBytes       int64                      `json:"hot_budget_bytes"`
	OldestUndelivered    int64                      `json:"oldest_undelivered_ingest,omitempty"`
	OldestArchivePending time.Time                  `json:"oldest_archive_pending,omitempty"`
	HotLimitBytes        int64                      `json:"hot_limit_bytes"`
	FreeBytes            uint64                     `json:"free_bytes"`
	DiskPressure         bool                       `json:"disk_pressure"`
	CapacityError        string                     `json:"capacity_error,omitempty"`
	Tasks                []TelemetryTaskDiagnostics `json:"tasks,omitempty"`
}

type TelemetryTaskDiagnostics struct {
	Name         string    `json:"name"`
	Due          time.Time `json:"due"`
	Attempts     int       `json:"attempts"`
	LeaseExpires time.Time `json:"lease_expires,omitempty"`
	LastError    string    `json:"last_error,omitempty"`
}

func (t *Telemetry) Diagnostics(ctx context.Context) (TelemetryDiagnostics, error) {
	var d TelemetryDiagnostics
	var err error
	d.HotLimitBytes = t.cfg.MaxHotBytes
	if capacityErr := t.CheckCapacity(ctx, false); capacityErr != nil {
		if ctx.Err() != nil {
			return d, ctx.Err()
		}
		d.CapacityError = capacityErr.Error()
		d.DiskPressure = true
	}
	t.capacity.Lock()
	d.FreeBytes = t.capacity.free
	t.capacity.Unlock()
	if d.Accepted, err = t.progress(ctx, t.s.reader(), "ingest"); err != nil {
		return d, err
	}
	if d.Summarized, err = t.progress(ctx, t.s.reader(), "summary"); err != nil {
		return d, err
	}
	if d.HotBudgetBytes, err = t.progress(ctx, t.s.reader(), "hot_bytes"); err != nil {
		return d, err
	}
	err = t.s.reader().QueryRowContext(ctx, "SELECT ingest_id FROM "+t.table("outbox")+" WHERE delivered=0 ORDER BY ingest_id LIMIT 1").Scan(&d.OldestUndelivered)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return d, err
	}
	var ns int64
	err = t.s.reader().QueryRowContext(ctx, "SELECT created FROM "+t.table("segments")+" WHERE state IN ('sealed','writing','archived','deleting') ORDER BY created,id LIMIT 1").Scan(&ns)
	if err == nil {
		d.OldestArchivePending = time.Unix(0, ns).UTC()
	} else if !errors.Is(err, sql.ErrNoRows) {
		return d, err
	}
	if d.Summarized > d.Accepted {
		return d, fmt.Errorf("telemetry progress inconsistent")
	}
	rows, err := t.s.reader().QueryContext(ctx, "SELECT task,due,attempts,expires,last_error FROM "+t.table("schedule")+" ORDER BY task LIMIT 65")
	if err != nil {
		return d, err
	}
	defer rows.Close()
	for rows.Next() {
		var task TelemetryTaskDiagnostics
		var due, expires int64
		if err = rows.Scan(&task.Name, &due, &task.Attempts, &expires, &task.LastError); err != nil {
			return d, err
		}
		task.Due = time.Unix(0, due).UTC()
		if expires > 0 {
			task.LeaseExpires = time.Unix(0, expires).UTC()
		}
		d.Tasks = append(d.Tasks, task)
		if len(d.Tasks) > 64 {
			return d, ErrTelemetryBackpressure
		}
	}
	if err = rows.Err(); err != nil {
		return d, err
	}
	return d, nil
}

// Receipts are pending handoff state, not a historical identity ledger. Once
// raw, chart and traffic handoff complete, retire them. A later network retry
// may add a duplicate chart sample; no payload hashes or cold proof scans are
// justified for best-effort monitoring. Migration alone retains its in-flight
// receipts until its durable window/global checkpoints prevent re-import.
func (t *Telemetry) PruneReceipts(ctx context.Context, now time.Time, limit int) (int, error) {
	if limit <= 0 || limit > 512 {
		return 0, ErrInvalidArgument
	}
	release, err := t.acquire(ctx)
	if err != nil {
		return 0, err
	}
	defer release()
	tx, err := t.begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, t.bind("SELECT r.ingest_id FROM "+t.table("receipts")+" r JOIN "+t.table("segments")+" s ON s.id=r.segment_id WHERE s.state IN ('complete','expired') AND (r.importing=0 OR EXISTS (SELECT 1 FROM "+t.table("progress")+" p WHERE p.task='legacy_migration_complete' AND p.value=1)) AND NOT EXISTS (SELECT 1 FROM "+t.table("outbox")+" o WHERE o.ingest_id=r.ingest_id AND o.delivered=0) ORDER BY r.received,r.ingest_id LIMIT ?"), limit)
	if err != nil {
		return 0, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			break
		}
		ids = append(ids, id)
	}
	readErr := rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	if readErr != nil {
		return 0, readErr
	}
	for _, id := range ids {
		if _, err = tx.ExecContext(ctx, t.bind("DELETE FROM "+t.table("outbox")+" WHERE ingest_id=? AND delivered=1"), id); err != nil {
			return 0, err
		}
		if _, err = tx.ExecContext(ctx, t.bind("DELETE FROM "+t.table("receipts")+" WHERE ingest_id=?"), id); err != nil {
			return 0, err
		}
	}
	return len(ids), tx.Commit()
}
