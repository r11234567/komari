package historyexport

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func backupFixtureQueue(t *testing.T, q *Queue) string {
	t.Helper()
	ctx := context.Background()
	dest := filepath.Join(t.TempDir(), "exports")
	release, err := LockMaintenance(ctx, q.Dir)
	if err != nil {
		t.Fatal(err)
	}
	err = q.BackupLocked(ctx, dest)
	release()
	if err != nil {
		t.Fatal(err)
	}
	return dest
}

func TestPortableExportBackupResumesEveryModeAndReady(t *testing.T) {
	for _, mode := range []string{"raw", "summary", "sample30s"} {
		t.Run(mode, func(t *testing.T) {
			w, j := workerFixture(t, mode)
			interrupted, cancel := context.WithCancel(context.Background())
			// Stop immediately after a durable checkpoint, without marking the task
			// failed. This is the same on-disk state as process interruption.
			w.Fault = func(stage string) error {
				if stage == "checkpoint-after-commit" {
					cancel()
					return context.Canceled
				}
				return nil
			}
			if err := w.Step(interrupted, j); !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			j, err := w.Queue.Get(context.Background(), j.Owner, j.ID)
			if err != nil {
				t.Fatal(err)
			}
			dest := backupFixtureQueue(t, w.Queue)
			dataDir := t.TempDir()
			if err = ValidateAndRebindBackup(context.Background(), dest, dataDir); err != nil {
				t.Fatal(err)
			}
			q, err := Open(dest)
			if err != nil {
				t.Fatal(err)
			}
			defer q.Close()
			resumed, err := q.Get(context.Background(), j.Owner, j.ID)
			if err != nil {
				t.Fatal(err)
			}
			if resumed.Source.DSN != filepath.Join(dataDir, "metrics.db") || resumed.Source.Telemetry.ArchiveDirectory != filepath.Join(dataDir, "telemetry") {
				t.Fatal("non-portable source", resumed.Source)
			}
			// Frozen inputs are self-contained; the old metric and archive locations
			// are deliberately unavailable at the rebound paths.
			restoredWorker := &Worker{Queue: q, Verify: w.Verify}
			result := finishWorker(t, restoredWorker, resumed)
			original := finishWorker(t, &Worker{Queue: w.Queue, Verify: w.Verify}, j)
			if result.SHA256 != original.SHA256 || result.WrittenRows != original.WrittenRows {
				t.Fatal("resumed backup differs", result, original)
			}
			ready := backupFixtureQueue(t, q)
			if err = ValidateAndRebindBackup(context.Background(), ready, t.TempDir()); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(filepath.Join(ready, result.ID+".csv"))
			if err != nil || info.Size() != result.OutputBytes {
				t.Fatal(info, err)
			}
		})
	}
}

func TestExportBackupRejectsSameLengthTamperAndMissingReady(t *testing.T) {
	for _, mutation := range []string{"tamper", "missing", "extra"} {
		t.Run(mutation, func(t *testing.T) {
			w, j := workerFixture(t, "raw")
			j = finishWorker(t, w, j)
			dest := backupFixtureQueue(t, w.Queue)
			p := filepath.Join(dest, j.ID+".csv")
			if mutation == "extra" {
				if err := os.WriteFile(filepath.Join(dest, "unverified.csv"), []byte("extra"), 0600); err != nil {
					t.Fatal(err)
				}
			} else if mutation == "missing" {
				if err := os.Remove(p); err != nil {
					t.Fatal(err)
				}
			} else {
				b, err := os.ReadFile(p)
				if err != nil {
					t.Fatal(err)
				}
				b[len(b)/2] ^= 1
				if err = os.WriteFile(p, b, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := ValidateAndRebindBackup(context.Background(), dest, t.TempDir()); err == nil {
				t.Fatal("corrupt backup accepted")
			}
		})
	}
}

func TestExportWorkLockAndRestoredQueueFence(t *testing.T) {
	q, err := Open(filepath.Join(t.TempDir(), "queue"))
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	release, err := q.guard(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
	_, err = LockMaintenance(ctx, q.Dir)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("backup raced worker files", err)
	}
	release()
	maintenance, err := LockMaintenance(context.Background(), q.Dir)
	if err != nil {
		t.Fatal(err)
	}
	readCtx, readCancel := context.WithTimeout(context.Background(), time.Second)
	if _, err = q.List(readCtx, "owner"); err != nil {
		t.Fatal("backup blocked status reads", err)
	}
	readCancel()
	maintenance()
	dest := backupFixtureQueue(t, q)
	if err = ValidateAndRebindBackup(context.Background(), dest, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	old := q.Dir + "-old"
	if err = os.Rename(q.Dir, old); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(dest, q.Dir); err != nil {
		t.Fatal(err)
	}
	if _, err = q.List(context.Background(), "owner"); !errors.Is(err, ErrQueueReplaced) {
		t.Fatal("old connection survived restore", err)
	}
	fresh, err := Open(q.Dir)
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	if _, err = fresh.List(context.Background(), "owner"); err != nil {
		t.Fatal(err)
	}
}

func TestPreManifestBackupPreservesFilesWithoutPublishingUnverifiedTasks(t *testing.T) {
	w, j := workerFixture(t, "raw")
	j = finishWorker(t, w, j)
	dest := backupFixtureQueue(t, w.Queue)
	if err := os.Remove(filepath.Join(dest, "backup-manifest.json")); err != nil {
		t.Fatal(err)
	}
	if err := ValidateAndRebindBackup(context.Background(), dest, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dest+"-legacy-unverified", j.ID+".csv")); err != nil {
		t.Fatal("legacy bytes were lost", err)
	}
	q, err := Open(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	jobs, err := q.List(context.Background(), j.Owner)
	if err != nil || len(jobs) != 0 {
		t.Fatal("unverified task became downloadable", jobs, err)
	}
}

func TestWorkerHeartbeatCanStopWhileMaintenanceOwnsLock(t *testing.T) {
	w, _ := workerFixture(t, "raw")
	release, err := LockMaintenance(context.Background(), w.Queue.Dir)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err = w.heartbeat(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
}
