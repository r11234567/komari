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

const (
	maintenanceCleanupInterval = 30 * time.Minute
	// The compact cron ticks every 30s. A pass that is allowed to run for most of
	// that interval leaves no idle time: the following tick finds the gate held,
	// gives up, and the pass after that starts immediately, so the process never
	// settles. Keep the budget a fraction of the tick so a pass finishes, yields
	// the writer, and leaves the box idle until the next tick.
	maintenanceRunBudget        = 8 * time.Second
	maintenanceCleanupBudget    = 3 * time.Second
	maintenanceCheckpointBudget = 2 * time.Second
	maintenanceMaxTasks         = 64
	// Cleanup steps admitted per pass. Retention has to keep pace with ingest
	// across every series of a metric, which a single step per pass cannot do.
	maintenanceCleanupQuota = 4
)

type maintenanceTaskKind uint8

const (
	maintenanceCompact maintenanceTaskKind = iota + 1
	maintenanceCleanup
	maintenanceCheckpoint
)

type maintenanceTask struct {
	kind       maintenanceTaskKind
	metricName string
	due        time.Time
	attempts   int
}

// maintenanceScheduler is an in-memory, deduplicating queue. A task is kept
// until it succeeds; a scheduler tick can therefore never lose a compaction
// just because the previous tick was still running.
type maintenanceScheduler struct {
	mu               sync.Mutex
	compactionQueue  []maintenanceTask
	cleanupQueue     []maintenanceTask
	queued           map[string]struct{}
	checkpointQueued bool
	nextCleanupAt    time.Time
}

var metricMaintenance maintenanceScheduler

// SchedulerResult describes work completed by one scheduler invocation.
type SchedulerResult struct {
	Compactions int
	Cleanups    int
	Checkpoints int
	Written     int
}

func resetMaintenanceScheduler() {
	metricMaintenance.mu.Lock()
	metricMaintenance.compactionQueue = nil
	metricMaintenance.cleanupQueue = nil
	metricMaintenance.queued = nil
	metricMaintenance.checkpointQueued = false
	metricMaintenance.nextCleanupAt = time.Time{}
	metricMaintenance.mu.Unlock()
}

func (s *maintenanceScheduler) enqueue(kind maintenanceTaskKind, metricName string, due time.Time) {
	if s.queued == nil {
		s.queued = make(map[string]struct{})
	}
	key := fmt.Sprintf("%d:%s", kind, metricName)
	if _, exists := s.queued[key]; exists {
		return
	}
	s.queued[key] = struct{}{}
	task := maintenanceTask{kind: kind, metricName: metricName, due: due}
	if kind == maintenanceCompact {
		s.compactionQueue = append(s.compactionQueue, task)
	} else {
		s.cleanupQueue = append(s.cleanupQueue, task)
	}
}

func (s *maintenanceScheduler) enqueueCheckpoint() {
	if s.checkpointQueued {
		return
	}
	s.checkpointQueued = true
}

func (s *maintenanceScheduler) pop(queue *[]maintenanceTask, now time.Time) (maintenanceTask, bool) {
	for index, task := range *queue {
		if task.due.After(now) {
			continue
		}
		*queue = append((*queue)[:index], (*queue)[index+1:]...)
		if task.kind != maintenanceCheckpoint {
			delete(s.queued, fmt.Sprintf("%d:%s", task.kind, task.metricName))
		}
		return task, true
	}
	return maintenanceTask{}, false
}

func maintenanceRetryAt(now time.Time, attempts int) time.Time {
	if attempts < 1 {
		attempts = 1
	}
	delay := time.Duration(1<<minInt(attempts-1, 5)) * time.Second
	if delay > 5*time.Minute {
		delay = 5 * time.Minute
	}
	return now.Add(delay)
}

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}

// RunMaintenance is the production maintenance entry point. Compaction tasks
// always drain before cleanup or WAL checkpoint tasks. Cleanup is bounded to a
// single metric per invocation, while checkpointing is admitted only when the
// WAL state raises an event or a prior checkpoint is due for retry.
func RunMaintenance(ctx context.Context, now time.Time) (SchedulerResult, error) {
	if !compactOperations.TryAcquire() {
		return SchedulerResult{}, ErrCompactInProgress
	}
	defer compactOperations.Release()
	if err := storeOperations.AcquireShared(ctx); err != nil {
		return SchedulerResult{}, fmt.Errorf("wait for metric store operation before maintenance: %w", err)
	}
	storeMu.RLock()
	activeStore := store
	storeMu.RUnlock()
	if activeStore == nil {
		storeOperations.ReleaseShared()
		return SchedulerResult{}, fmt.Errorf("metric store not initialized")
	}
	defs, err := activeStore.ListMetrics(ctx)
	if err == nil {
		defs = compactableMetricDefinitions(activeStore, defs)
	}
	storeOperations.ReleaseShared()
	if err != nil {
		return SchedulerResult{}, err
	}

	now = now.UTC()
	metricMaintenance.mu.Lock()
	for _, def := range defs {
		metricMaintenance.enqueue(maintenanceCompact, def.Name, now)
	}
	if metricMaintenance.nextCleanupAt.IsZero() || !now.Before(metricMaintenance.nextCleanupAt) {
		for _, def := range defs {
			metricMaintenance.enqueue(maintenanceCleanup, def.Name, now)
		}
		metricMaintenance.nextCleanupAt = now.Add(maintenanceCleanupInterval)
	}
	// One fair pass is admitted per invocation. Tasks that make progress are
	// requeued at the tail for the next pass, so a backlog does not starve
	// cleanup/checkpoint work and a metric is not tied to the cron interval.
	compactionQuota := len(metricMaintenance.compactionQueue)
	metricMaintenance.mu.Unlock()

	deadline := time.Now().Add(MaintenanceTimeout(time.Now().UTC()))
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	result := SchedulerResult{}
	var errs []error
	for result.Compactions+result.Cleanups+result.Checkpoints < maintenanceMaxTasks && time.Now().Before(deadline) {
		metricMaintenance.mu.Lock()
		var task maintenanceTask
		var ok bool
		if result.Compactions < compactionQuota {
			task, ok = metricMaintenance.pop(&metricMaintenance.compactionQueue, now)
		}
		metricMaintenance.mu.Unlock()
		if ok {
			if err := storeOperations.AcquireShared(ctx); err != nil {
				return result, fmt.Errorf("wait for compaction maintenance slot: %w", err)
			}
			storeMu.RLock()
			taskStore := store
			storeMu.RUnlock()
			if taskStore == nil {
				storeOperations.ReleaseShared()
				return result, fmt.Errorf("metric store not initialized")
			}
			stepIndex := result.Compactions
			if len(defs) > 0 {
				stepIndex %= len(defs)
			}
			beginCompactStep(taskStore.Driver(), task.metricName, stepIndex, len(defs), time.Now().UTC())
			written, compactErr := taskStore.CompactMetricStep(ctx, task.metricName, now)
			storeOperations.ReleaseShared()
			compactionRetry := false
			if metric.IsDigestHandoffDeferred(compactErr) {
				handleDigestHandoffDeferred(task.metricName, compactErr, time.Now().UTC())
				compactionRetry = true
				compactErr = nil
			} else if errors.Is(compactErr, metric.ErrNotFound) {
				// A reload or definition deletion can leave a stale queued task.
				// Drop it instead of retrying a metric that no longer exists.
				compactErr = nil
			} else if compactErr == nil {
				clearDigestHandoffDeferred(task.metricName)
			}
			if compactErr != nil && !errors.Is(compactErr, metric.ErrNotFound) {
				errs = append(errs, fmt.Errorf("compact metric %q: %w", task.metricName, compactErr))
				task.attempts++
				task.due = maintenanceRetryAt(now, task.attempts)
				metricMaintenance.mu.Lock()
				metricMaintenance.queued[fmt.Sprintf("%d:%s", task.kind, task.metricName)] = struct{}{}
				metricMaintenance.compactionQueue = append(metricMaintenance.compactionQueue, task)
				metricMaintenance.mu.Unlock()
			} else if compactErr == nil && (written > 0 || compactionRetry) {
				// A successful bounded step with output implies more historical
				// work may remain. Keep it moving without waiting for cron.
				task.attempts = 0
				task.due = now
				metricMaintenance.mu.Lock()
				metricMaintenance.queued[fmt.Sprintf("%d:%s", task.kind, task.metricName)] = struct{}{}
				metricMaintenance.compactionQueue = append(metricMaintenance.compactionQueue, task)
				metricMaintenance.mu.Unlock()
			}
			result.Compactions++
			result.Written += written
			cycleCompleted := !maintenanceCompactionPending(now)
			finishCompactStep(written, cycleCompleted, compactErr, time.Now().UTC())
			continue
		}

		// At most one checkpoint per invocation. maintenanceCheckpointDue re-reads
		// the WAL size against a fixed `now`, so a checkpoint that succeeds without
		// shrinking the WAL - normal when a reader still pins an old snapshot -
		// reports due again immediately and would otherwise spin here until the
		// task or deadline cap is hit.
		if result.Checkpoints == 0 && (result.Compactions == 0 || result.Compactions >= compactionQuota || !maintenanceCompactionPending(now)) {
			if err := storeOperations.AcquireShared(ctx); err != nil {
				return result, fmt.Errorf("wait for checkpoint maintenance slot: %w", err)
			}
			storeMu.RLock()
			taskStore := store
			storeMu.RUnlock()
			checkpointDue := taskStore != nil && maintenanceCheckpointDue(ctx, taskStore, now)
			if checkpointDue {
				metricMaintenance.mu.Lock()
				metricMaintenance.enqueueCheckpoint()
				checkpointQueued := metricMaintenance.checkpointQueued
				if checkpointQueued {
					metricMaintenance.checkpointQueued = false
				}
				metricMaintenance.mu.Unlock()
				if checkpointQueued {
					checkpointCtx, cancel := context.WithTimeout(ctx, maintenanceCheckpointBudget)
					checkpointErr := taskStore.CheckpointWAL(checkpointCtx)
					cancel()
					recordCheckpointResult(taskStore.Driver(), checkpointErr, time.Now().UTC())
					result.Checkpoints++
					if checkpointErr != nil {
						errs = append(errs, fmt.Errorf("checkpoint metric WAL: %w", checkpointErr))
					}
					storeOperations.ReleaseShared()
					continue
				}
			}
			storeOperations.ReleaseShared()
		}

		metricMaintenance.mu.Lock()
		cleanupTask, cleanupOK := metricMaintenance.pop(&metricMaintenance.cleanupQueue, now)
		metricMaintenance.mu.Unlock()
		if !cleanupOK {
			break
		}
		cleanupCtx, cancel := context.WithTimeout(ctx, maintenanceCleanupBudget)
		if err := storeOperations.AcquireShared(cleanupCtx); err != nil {
			cancel()
			errs = append(errs, fmt.Errorf("wait for cleanup maintenance slot: %w", err))
			break
		}
		storeMu.RLock()
		taskStore := store
		storeMu.RUnlock()
		if taskStore == nil {
			storeOperations.ReleaseShared()
			cancel()
			return result, fmt.Errorf("metric store not initialized")
		}
		deleted, cleanupComplete, cleanupErr := taskStore.CleanupExpiredMetricStep(cleanupCtx, cleanupTask.metricName, now)
		storeOperations.ReleaseShared()
		cancel()
		result.Cleanups++
		if cleanupErr != nil {
			errs = append(errs, fmt.Errorf("clean up metric %q: %w", cleanupTask.metricName, cleanupErr))
			cleanupTask.attempts++
			cleanupTask.due = maintenanceRetryAt(now, cleanupTask.attempts)
			metricMaintenance.mu.Lock()
			metricMaintenance.queued[fmt.Sprintf("%d:%s", cleanupTask.kind, cleanupTask.metricName)] = struct{}{}
			metricMaintenance.cleanupQueue = append(metricMaintenance.cleanupQueue, cleanupTask)
			metricMaintenance.mu.Unlock()
		} else if !cleanupComplete || deleted > 0 {
			// A successful slice may have removed only part of the expired
			// history. Requeue immediately so cleanup makes steady progress
			// instead of waiting for the next 30-minute sweep.
			cleanupTask.attempts = 0
			cleanupTask.due = now
			metricMaintenance.mu.Lock()
			metricMaintenance.queued[fmt.Sprintf("%d:%s", cleanupTask.kind, cleanupTask.metricName)] = struct{}{}
			metricMaintenance.cleanupQueue = append(metricMaintenance.cleanupQueue, cleanupTask)
			metricMaintenance.mu.Unlock()
		}
		// Compaction is popped first on every iteration and re-enqueued for all
		// metrics at the start of each pass, so admitting a few cleanup steps here
		// cannot starve it. The deadline and task cap still bound the pass.
		if result.Cleanups >= maintenanceCleanupQuota {
			break
		}
	}
	if len(errs) > 0 {
		logger.Warnf("metricstore", "maintenance scheduler completed with %d deferred errors", len(errs))
	}
	return result, errors.Join(errs...)
}

func maintenanceCompactionPending(now time.Time) bool {
	metricMaintenance.mu.Lock()
	defer metricMaintenance.mu.Unlock()
	for _, task := range metricMaintenance.compactionQueue {
		if !task.due.After(now) {
			return true
		}
	}
	return false
}

func maintenanceCheckpointDue(ctx context.Context, activeStore *metric.Store, now time.Time) bool {
	pending, quickDue, fullDue := checkpointRetryState(now)
	if pending {
		// A failed checkpoint is retried on its recorded backoff even when the
		// WAL remains above the threshold; repeated ticks must not hot-loop.
		return quickDue || fullDue
	}
	if activeStore.Driver() != metric.DriverSQLite {
		return false
	}
	files, err := activeStore.SQLiteFiles(ctx)
	return err == nil && files.WAL >= metricWALCheckpointLimit
}
