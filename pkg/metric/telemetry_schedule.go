package metric

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ScheduleTask is a small durable control row. Data cursors remain in the
// transaction which owns their work; Cursor is only for fair traversal (e.g.
// metric definitions), never a substitute for a committed ingestion cursor.
type ScheduleTask struct {
	Name     string    `json:"name"`
	Due      time.Time `json:"due"`
	Attempts int       `json:"attempts"`
	Cursor   string    `json:"cursor,omitempty"`
	token    string
}

type ScheduleOutcome struct {
	Progress int
	Delay    time.Duration
	Cursor   string
}

func (t *Telemetry) EnsureSchedule(ctx context.Context, tasks []ScheduleTask) error {
	if len(tasks) > 64 {
		return ErrInvalidArgument
	}
	release, err := t.acquire(ctx)
	if err != nil {
		return err
	}
	defer release()
	tx, err := t.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, task := range tasks {
		if task.Name == "" || len(task.Name) > 64 {
			return ErrInvalidArgument
		}
		q := "INSERT INTO " + t.table("schedule") + " (task,due,attempts,scan_after,owner,expires,last_error) VALUES (?,?,0,'','',0,'')"
		if t.s.cfg.Driver == DriverMySQL {
			q += " ON DUPLICATE KEY UPDATE task=task"
		} else {
			q += " ON CONFLICT(task) DO NOTHING"
		}
		if _, err = tx.ExecContext(ctx, t.bind(q), task.Name, task.Due.UnixNano()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// An input notification may shorten an idle wait, never a failure backoff or
// an active lease. Failure deadlines survive process restart and config reload.
func (t *Telemetry) ExpediteSchedule(ctx context.Context, names []string, at time.Time) error {
	for _, name := range names {
		if _, err := t.s.db.ExecContext(ctx, t.bind("UPDATE "+t.table("schedule")+" SET due=? WHERE task=? AND due>? AND attempts=0 AND expires<=?"), at.UnixNano(), name, at.UnixNano(), at.UnixNano()); err != nil {
			return err
		}
	}
	return nil
}

func telemetryRetryDelay(attempt int) time.Duration {
	steps := []time.Duration{time.Minute, 2 * time.Minute, 5 * time.Minute, 15 * time.Minute, 30 * time.Minute}
	base := steps[max(0, min(attempt-1, len(steps)-1))]
	var b [1]byte
	_, _ = rand.Read(b[:])
	return base + time.Duration(float64(base)*float64(b[0])/255*0.2)
}

// DispatchSchedule runs at most one due task under a database lease and a local
// single-worker gate. A killed worker's claimed task becomes due after its
// lease; a failed task is delayed from completion, not its previous start.
func (t *Telemetry) DispatchSchedule(ctx context.Context, now time.Time, run func(context.Context, ScheduleTask) (ScheduleOutcome, error)) (ScheduleTask, ScheduleOutcome, bool, error) {
	var task ScheduleTask
	var out ScheduleOutcome
	select {
	case t.scheduleGate <- struct{}{}:
		defer func() { <-t.scheduleGate }()
	default:
		return task, out, false, ErrTelemetryBackpressure
	}
	if err := t.claimWork(ctx, "maintenance"); err != nil {
		return task, out, false, err
	}
	defer t.releaseWork("maintenance")
	release, err := t.acquire(ctx)
	if err != nil {
		return task, out, false, err
	}
	tx, err := t.begin(ctx)
	if err != nil {
		release()
		return task, out, false, err
	}
	var due int64
	err = tx.QueryRowContext(ctx, t.bind("SELECT task,due,attempts,scan_after FROM "+t.table("schedule")+" WHERE due<=? AND expires<=? ORDER BY due,task LIMIT 1"), now.UnixNano(), now.UnixNano()).Scan(&task.Name, &due, &task.Attempts, &task.Cursor)
	if err != nil {
		tx.Rollback()
		release()
		if errors.Is(err, sql.ErrNoRows) {
			return task, out, false, nil
		}
		return task, out, false, err
	}
	task.Due = time.Unix(0, due).UTC()
	task.token = telemetryID()
	_, err = tx.ExecContext(ctx, t.bind("UPDATE "+t.table("schedule")+" SET owner=?,expires=? WHERE task=?"), task.token, time.Now().Add(t.cfg.WorkLease).UnixNano(), task.Name)
	if err == nil {
		err = tx.Commit()
	} else {
		tx.Rollback()
	}
	release()
	if err != nil {
		return task, out, false, err
	}
	out, workErr := run(ctx, task)
	finished := time.Now().UTC()
	delay := out.Delay
	if delay < 200*time.Millisecond {
		delay = 200 * time.Millisecond
	}
	attempts := 0
	message := ""
	if workErr != nil {
		out.Cursor = task.Cursor
		attempts = min(task.Attempts+1, 1000)
		delay = telemetryRetryDelay(attempts)
		message = workErr.Error()
		if len(message) > 512 {
			message = message[:512]
		}
	}
	// A request timeout must not erase the failure deadline. This short control
	// transaction is independent of the cancelled data operation.
	finishCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	r, finishErr := t.s.db.ExecContext(finishCtx, t.bind("UPDATE "+t.table("schedule")+" SET due=?,attempts=?,scan_after=?,owner='',expires=0,last_error=? WHERE task=? AND owner=?"), finished.Add(delay).UnixNano(), attempts, out.Cursor, message, task.Name, task.token)
	if finishErr == nil {
		n, e := r.RowsAffected()
		if e != nil {
			finishErr = e
		} else if n != 1 {
			finishErr = fmt.Errorf("telemetry scheduled task lease lost: %s", task.Name)
		}
	}
	return task, out, true, errors.Join(workErr, finishErr)
}

func (t *Telemetry) ScheduleDelay(ctx context.Context, now time.Time) (time.Duration, error) {
	var next sql.NullInt64
	err := t.s.reader().QueryRowContext(ctx, "SELECT MIN(CASE WHEN due>expires THEN due ELSE expires END) FROM "+t.table("schedule")).Scan(&next)
	if err != nil {
		return time.Second, err
	}
	if !next.Valid {
		return time.Minute, nil
	}
	return max(200*time.Millisecond, time.Unix(0, next.Int64).Sub(now)), nil
}

// Changes is a coalesced hint. It is safe to lose hints: acceptance and all
// work progress are durable, and the scheduler rechecks indexed cursors.
func (t *Telemetry) Changes() <-chan struct{} { return t.changed }

func (t *Telemetry) notify() {
	select {
	case t.changed <- struct{}{}:
	default:
	}
}
