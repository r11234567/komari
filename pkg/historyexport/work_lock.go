package historyexport

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/komari-monitor/komari/internal/datadir"
)

var ErrQueueReplaced = errors.New("export queue was restored or replaced; restart this process")

// LockMaintenance excludes worker file changes while a backup snapshot or
// restore directory exchange is in progress. Callers must release on errors.
func LockMaintenance(ctx context.Context, dir string) (func(), error) {
	return acquireWorkLock(ctx, dir, true)
}

// LockGenerationPublication keeps worker transactions on the selected
// generation until the panel publishes the next pointer.
func LockGenerationPublication(ctx context.Context, root string) (func(), error) {
	return lockGeneration(ctx, root, true)
}

func lockGeneration(ctx context.Context, root string, exclusive bool) (func(), error) {
	dir := filepath.Join(root, ".restore-lock", "publication")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return acquireWorkLock(ctx, dir, exclusive)
}

// OpenWorkerQueue resolves the deployed --queue path on every worker reopen.
// Explicit standalone queue directories keep their existing behavior.
func OpenWorkerQueue(ctx context.Context, dir string) (*Queue, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if filepath.Base(dir) != "exports-v2" {
		return Open(dir)
	}
	root := filepath.Dir(dir)
	release, err := lockGeneration(ctx, root, false)
	if err != nil {
		return nil, err
	}
	defer release()
	layout, err := datadir.Open(root)
	if err != nil {
		return nil, err
	}
	q, err := Open(layout.Resolve(dir))
	if err != nil {
		return nil, err
	}
	q.generationRoot = root
	return q, nil
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
	if q.generationRoot != "" {
		layout, err := datadir.Open(q.generationRoot)
		if err != nil || filepath.Join(layout.Dir, "exports-v2") != q.Dir {
			return errors.Join(ErrQueueReplaced, err)
		}
	}
	info, err := os.Stat(filepath.Join(q.Dir, "queue.db"))
	if err != nil || !os.SameFile(q.identity, info) {
		return ErrQueueReplaced
	}
	return nil
}
func (q *Queue) guard(ctx context.Context) (func(), error) {
	releaseGeneration := func() {}
	if q.generationRoot != "" {
		var err error
		releaseGeneration, err = lockGeneration(ctx, q.generationRoot, false)
		if err != nil {
			return nil, err
		}
	}
	release, err := acquireWorkLock(ctx, q.Dir, false)
	if err != nil {
		releaseGeneration()
		return nil, err
	}
	if err = q.checkGeneration(); err != nil {
		release()
		releaseGeneration()
		return nil, err
	}
	return func() {
		release()
		releaseGeneration()
	}, nil
}
