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
	// Raw availability has its own lifetime. Publish the floor in the same
	// transaction as expiry, before removing any bytes, even with no candidates.
	// Chart retention must never delay this boundary.
	var floor int64
	if err = tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(value),0) FROM "+t.table("progress")+" WHERE task='raw_history_floor'").Scan(&floor); err != nil {
		return false, err
	}
	if cutoff := now.Add(-retention).UnixNano(); cutoff > floor {
		if err = t.setProgress(ctx, tx, "raw_history_floor", cutoff); err != nil {
			return false, err
		}
	}
	if _, err = tx.ExecContext(ctx, t.bind("DELETE FROM "+t.table("leases")+" WHERE expires<=?"), now.UnixNano()); err != nil {
		return false, err
	}
	var id string
	err = tx.QueryRowContext(ctx, t.bind("SELECT a.id FROM "+t.table("segments")+" a WHERE (a.state='expiring' OR (a.state='complete' AND a.created<?)) AND NOT EXISTS (SELECT 1 FROM "+t.table("leases")+" l WHERE l.segment_id=a.id AND l.expires>?) ORDER BY a.created,a.id LIMIT 1"), now.Add(-retention).UnixNano(), now.UnixNano()).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, tx.Commit()
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
	var owners int64
	if seg.ArchiveFile != "" {
		if err = t.s.reader().QueryRowContext(ctx, t.bind("SELECT COUNT(*) FROM "+t.table("segments")+" WHERE archive_file=? AND id<>? AND state<>'expired'"), seg.ArchiveFile, id).Scan(&owners); err != nil {
			return false, err
		}
	}
	if owners == 0 {
		if err = t.root.Remove(path); err != nil && !os.IsNotExist(err) {
			return false, err
		}
		d, e := t.root.Open(filepath.Dir(path))
		if e != nil {
			return false, e
		}
		e = d.Sync()
		d.Close()
		if e != nil {
			return false, e
		}
	}
	if err = t.fault("expire-after-remove"); err != nil {
		return false, err
	}
	_, err = t.s.db.ExecContext(ctx, t.bind("UPDATE "+t.table("segments")+" SET state='expired' WHERE id=? AND state='expiring'"), id)
	return err == nil, err
}
