package historyexport

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"
)

var ErrQueueReplaced = errors.New("export queue was restored or replaced; restart this process")

// LockMaintenance excludes worker file changes while a backup snapshot or
// restore directory exchange is in progress. Callers must release on errors.
func LockMaintenance(ctx context.Context, dir string) (func(), error) {
	return acquireWorkLock(ctx, dir, true)
}
func acquireWorkLock(ctx context.Context, dir string, exclusive bool) (func(), error) {
	f, err := os.OpenFile(filepath.Join(dir, "work.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	for {
		if err = ctx.Err(); err != nil {
			f.Close()
			return nil, err
		}
		if err = tryWorkLock(f, exclusive); err == nil {
			return func() { unlockWork(f); f.Close() }, nil
		}
		if !workLockBusy(err) {
			f.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}

// A queue connection is tied to its original database inode. A restored copy
// may have identical source IDs, so comparing telemetry epoch alone is unsafe.
func (q *Queue) checkGeneration() error {
	info, err := os.Stat(filepath.Join(q.Dir, "queue.db"))
	if err != nil || !os.SameFile(q.identity, info) {
		return ErrQueueReplaced
	}
	return nil
}
func (q *Queue) guard(ctx context.Context) (func(), error) {
	release, err := acquireWorkLock(ctx, q.Dir, false)
	if err != nil {
		return nil, err
	}
	if err = q.checkGeneration(); err != nil {
		release()
		return nil, err
	}
	return release, nil
}
