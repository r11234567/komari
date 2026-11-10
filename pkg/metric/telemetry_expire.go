package metric

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// ExpireArchiveNext removes one fully handed-off immutable file. The catalog
// tombstone survives removal, and a crash in expiring is resumed. Statistical
// cells and independent business ledgers retain their own lifetimes.
func (t *Telemetry) ExpireArchiveNext(ctx context.Context, now time.Time, retention time.Duration) (bool, error) {
	if retention <= 0 {
		return false, ErrInvalidArgument
	}
	select {
	case t.archiveGate <- struct{}{}:
		defer func() { <-t.archiveGate }()
	case <-ctx.Done():
		return false, ctx.Err()
	}
	release, err := t.acquire(ctx)
	if err != nil {
		return false, err
	}
	defer release()
	if err = t.claimWork(ctx, "archive"); err != nil {
		return false, err
	}
	defer t.releaseWork("archive")
	tx, err := t.begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, t.bind("DELETE FROM "+t.table("leases")+" WHERE expires<=?"), now.UnixNano()); err != nil {
		return false, err
	}
	var id string
	err = tx.QueryRowContext(ctx, t.bind("SELECT a.id FROM "+t.table("segments")+" a WHERE (a.state='expiring' OR (a.state='complete' AND a.created<?)) AND NOT EXISTS (SELECT 1 FROM "+t.table("leases")+" l WHERE l.segment_id=a.id AND l.expires>?) ORDER BY a.created,a.id LIMIT 1"), now.Add(-retention).UnixNano(), now.UnixNano()).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	seg, err := t.segment(ctx, tx, id)
	if err != nil {
		return false, err
	}
	if err = t.renewWork(ctx, tx, "archive"); err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, t.bind("UPDATE "+t.table("segments")+" SET state='expiring' WHERE id=?"), id); err != nil {
		return false, err
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	if err = t.fault("expire-before-remove"); err != nil {
		return false, err
	}
	if err = t.renewWork(ctx, t.s.db, "archive"); err != nil {
		return false, err
	}
	path := t.segmentPath(seg)
	if err = t.root.Remove(path); err != nil && !os.IsNotExist(err) {
		return false, err
	}
	d, err := t.root.Open(filepath.Dir(path))
	if err != nil {
		return false, err
	}
	err = d.Sync()
	d.Close()
	if err != nil {
		return false, err
	}
	if err = t.fault("expire-after-remove"); err != nil {
		return false, err
	}
	_, err = t.s.db.ExecContext(ctx, t.bind("UPDATE "+t.table("segments")+" SET state='expired' WHERE id=? AND state='expiring'"), id)
	return err == nil, err
}
