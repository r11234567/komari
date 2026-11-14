package metricstore

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/komari-monitor/komari/pkg/metric"
	logger "github.com/komari-monitor/komari/utils/log"
)

var telemetryScheduleState struct {
	sync.Mutex
	engine *metric.Telemetry
}
var telemetryWorker struct {
	sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

func ensureTelemetrySchedule(ctx context.Context, t *metric.Telemetry, now time.Time) error {
	telemetryScheduleState.Lock()
	defer telemetryScheduleState.Unlock()
	if telemetryScheduleState.engine == t {
		return nil
	}
	var tasks []metric.ScheduleTask
	for _, name := range []string{"traffic", "summary", "seal", "archive", "delete", "tiers", "summaries", "expire", "receipts", "erased", "checkpoint", "catalog", "ledger", "gaps"} {
		tasks = append(tasks, metric.ScheduleTask{Name: name, Due: now})
	}
	if err := t.EnsureSchedule(ctx, tasks); err != nil {
		return err
	}
	telemetryScheduleState.engine = t
	return nil
}

// StartTelemetryScheduler owns one background worker. Input hints wake only the
// summary/outbox tasks; deadlines and leases are persistent in the metric SQL.
// The loop holds no Store lifecycle lease while sleeping or waiting for input.
func StartTelemetryScheduler() {
	telemetryWorker.Lock()
	defer telemetryWorker.Unlock()
	if telemetryWorker.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	telemetryWorker.cancel, telemetryWorker.done = cancel, done
	go func() { defer close(done); telemetrySchedulerLoop(ctx) }()
}

func StopTelemetryScheduler(ctx context.Context) error {
	telemetryWorker.Lock()
	cancel, done := telemetryWorker.cancel, telemetryWorker.done
	telemetryWorker.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	select {
	case <-done:
	case <-ctx.Done():
		return ctx.Err()
	}
	telemetryWorker.Lock()
	if telemetryWorker.done == done {
		telemetryWorker.cancel = nil
		telemetryWorker.done = nil
	}
	telemetryWorker.Unlock()
	return nil
}

func telemetrySchedulerLoop(ctx context.Context) {
	input := true
	for ctx.Err() == nil {
		started := time.Now()
		wait := time.Minute
		var changed <-chan struct{}
		active := false
		s, release, err := AcquireStore(ctx)
		if err == nil {
			if t := s.Telemetry(); t != nil {
				active = true
				changed = t.Changes()
				err = ensureTelemetrySchedule(ctx, t, started)
				if err == nil && input {
					err = t.ExpediteSchedule(ctx, []string{"summary", "traffic"}, started)
				}
				input = false
			}
			release()
		}
		if err == nil && active {
			workCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
			_, err = RunMaintenance(workCtx, started)
			cancel()
		}
		if err != nil && ctx.Err() == nil && !errors.Is(err, ErrCompactInProgress) {
			logger.Warnf("metricstore", "Telemetry maintenance deferred: %v", err)
		}
		// A short sleep after every committed work slice reserves foreground CPU.
		// Long streaming archive work yields internally; do not multiply its
		// deliberate I/O pauses into another long period of inactivity.
		yield := max(100*time.Millisecond, 3*time.Since(started))
		if yield > time.Second {
			yield = time.Second
		}
		if s, r, e := AcquireStore(ctx); e == nil {
			if t := s.Telemetry(); t != nil {
				if d, e := t.ScheduleDelay(ctx, time.Now()); e == nil {
					wait = min(wait, d)
				}
			}
			r()
		}
		if err != nil {
			wait = max(wait, time.Second)
		}
		if !telemetryWait(ctx, yield) {
			return
		}
		timer := time.NewTimer(max(0, wait-yield))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-changed:
			timer.Stop()
			input = true
		case <-timer.C:
		}
	}
}

func telemetryWait(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func runTelemetryPass(ctx context.Context, s *metric.Store, now time.Time) (SchedulerResult, error) {
	var result SchedulerResult
	t := s.Telemetry()
	if err := ensureTelemetrySchedule(ctx, t, now); err != nil {
		return result, err
	}
	task, out, ran, err := t.DispatchSchedule(ctx, now, func(ctx context.Context, task metric.ScheduleTask) (metric.ScheduleOutcome, error) {
		out := metric.ScheduleOutcome{Cursor: task.Cursor, Delay: time.Hour}
		chunk := max(8, 128>>min(task.Attempts, 4))
		budget := 2 * time.Second
		if task.Name == "archive" || task.Name == "delete" {
			budget = 40 * time.Second
		}
		work, cancel := context.WithTimeout(ctx, budget)
		defer cancel()
		began := time.Now()
		work = metric.WithTelemetryWorkYield(work, func(ctx context.Context) error {
			if time.Since(began) < 200*time.Millisecond {
				return nil
			}
			if !telemetryWait(ctx, 600*time.Millisecond) {
				return ctx.Err()
			}
			began = time.Now()
			return nil
		})
		var err error
		switch task.Name {
		case "summary":
			drained := false
			for time.Since(began) < 200*time.Millisecond {
				var n int
				n, err = t.SummarizeNext(work)
				out.Progress += n
				drained = err == nil && n == 0
				if err != nil || n == 0 {
					break
				}
			}
			if out.Progress > 0 {
				out.Delay = 200 * time.Millisecond
				if drained {
					err = errors.Join(err, t.ExpediteSchedule(work, []string{"tiers"}, now))
				}
			}
		case "traffic":
			if c := activeTelemetryConsumer.Load(); c != nil && c.apply != nil {
				out.Progress, err = t.DeliverTraffic(work, chunk, c.apply)
			}
			if out.Progress > 0 {
				out.Delay = 200 * time.Millisecond
			}
		case "seal":
			var n int64
			n, err = t.SealDue(work, now)
			out.Progress = int(n)
			out.Delay = time.Minute
			if n > 0 {
				err = errors.Join(err, t.ExpediteSchedule(work, []string{"tiers", "archive", "delete"}, time.Now()))
			}
		case "archive":
			var ok bool
			ok, err = t.ArchiveNext(work)
			out.Delay = time.Minute
			if ok {
				out.Progress = 1
				out.Delay = time.Second
				err = errors.Join(err, t.ExpediteSchedule(work, []string{"delete"}, time.Now()))
			}
		case "delete":
			out.Progress, err = t.DeleteArchivedChunk(work, now, chunk)
			out.Delay = time.Minute
			if out.Progress > 0 {
				out.Delay = 200 * time.Millisecond
			}
		case "tiers":
			out.Progress, err = t.PromoteSummariesChunk(work, now, chunk)
			if out.Progress > 0 {
				out.Delay = time.Second
			} else if err == nil {
				out.Delay, err = t.NextSummaryPromotion(work, now)
			}
		case "gaps":
			out.Progress, out.Cursor, err = t.CompactGapsChunk(work, task.Cursor, chunk)
			out.Delay = 24 * time.Hour
			if out.Progress > 0 {
				out.Delay = time.Second
			}
		case "summaries":
			var defs []metric.Definition
			defs, err = s.ListMetrics(work)
			if err == nil {
				var chosen *metric.Definition
				for i := range defs {
					if defs[i].Name > task.Cursor && (chosen == nil || defs[i].Name < chosen.Name) {
						chosen = &defs[i]
					}
				}
				if chosen == nil {
					out.Cursor = ""
				} else {
					if chosen.RetentionDays > 0 {
						out.Progress, err = t.PruneSummaries(work, chosen.Name, time.Duration(chosen.RetentionDays)*24*time.Hour, now, chunk)
					}
					if out.Progress < chunk {
						out.Cursor = chosen.Name
					}
					out.Delay = time.Second
				}
			}
		case "expire":
			days := 30
			if cfg := activeTelemetrySettings.Load(); cfg != nil && cfg.ArchiveRetentionDays > 0 {
				days = cfg.ArchiveRetentionDays
			}
			var ok bool
			ok, err = t.ExpireArchiveNext(work, now, time.Duration(days)*24*time.Hour)
			if ok {
				out.Progress = 1
				out.Delay = time.Second
			}
		case "receipts":
			out.Progress, err = t.PruneReceipts(work, now, chunk)
			if out.Progress > 0 {
				out.Delay = time.Second
			}
		case "erased":
			out.Progress, err = t.PruneErasedChunk(work, chunk)
			if out.Progress > 0 {
				out.Delay = time.Second
			}
		case "catalog":
			out.Progress, err = t.PruneCatalogChunk(work, now, chunk)
			if out.Progress > 0 {
				out.Delay = time.Second
			}
		case "ledger":
			if c := activeTelemetryLedgerGC.Load(); c != nil && c.apply != nil {
				out.Progress, err = c.apply(work, now, chunk)
			}
			if out.Progress > 0 {
				out.Delay = time.Second
			}
		case "checkpoint":
			out.Delay = time.Minute
			if s.Driver() == metric.DriverSQLite {
				var files metric.SQLiteFileSizes
				files, err = s.SQLiteFiles(work)
				if err == nil && files.WAL >= metricWALCheckpointLimit {
					err = s.CheckpointWAL(work)
					if err == nil {
						out.Progress = 1
					}
				}
			}
		default:
			err = fmt.Errorf("unknown telemetry maintenance task %q", task.Name)
		}
		return out, err
	})
	if ran {
		switch task.Name {
		case "summary":
			result.Compactions = 1
			result.Written = out.Progress
		case "checkpoint":
			result.Checkpoints = out.Progress
		default:
			result.Cleanups = out.Progress
		}
	}
	return result, err
}
