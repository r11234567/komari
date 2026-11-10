package metric

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// A database lease also excludes a second Store/process during reload. Work
// files are attempt-specific; an expired worker cannot replace a published file
// because publication locks and rechecks this lease in the catalog transaction.
func (t *Telemetry) claimWork(ctx context.Context, task string) error {
	tx, err := t.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var owner string
	var expires int64
	err = tx.QueryRowContext(ctx, t.bind("SELECT owner,expires FROM "+t.table("work")+" WHERE task=?"), task).Scan(&owner, &expires)
	now := time.Now()
	next := now.Add(t.cfg.WorkLease).UnixNano()
	if errors.Is(err, sql.ErrNoRows) {
		_, err = tx.ExecContext(ctx, t.bind("INSERT INTO "+t.table("work")+" (task,owner,expires) VALUES (?,?,?)"), task, t.owner, next)
	} else if err == nil {
		if expires > now.UnixNano() && owner != t.owner {
			return ErrTelemetryBackpressure
		}
		var r sql.Result
		r, err = tx.ExecContext(ctx, t.bind("UPDATE "+t.table("work")+" SET owner=?,expires=? WHERE task=? AND owner=? AND expires=?"), t.owner, next, task, owner, expires)
		if err == nil {
			var n int64
			n, err = r.RowsAffected()
			if err == nil && n != 1 {
				return ErrTelemetryBackpressure
			}
		}
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (t *Telemetry) renewWork(ctx context.Context, q execer, task string) error {
	r, err := q.ExecContext(ctx, t.bind("UPDATE "+t.table("work")+" SET expires=? WHERE task=? AND owner=?"), time.Now().Add(t.cfg.WorkLease).UnixNano(), task, t.owner)
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrTelemetryBackpressure
	}
	return nil
}
func (t *Telemetry) releaseWork(task string) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	t.s.db.ExecContext(ctx, t.bind("DELETE FROM "+t.table("work")+" WHERE task=? AND owner=?"), task, t.owner)
}
