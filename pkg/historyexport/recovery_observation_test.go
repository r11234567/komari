package historyexport

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

func TestRecoveryReportsLivePrefixValidationWithoutAdvancingExport(t *testing.T) {
	for _, mode := range []string{"raw", "summary", "sample30s"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			w, j := workerFixture(t, mode)
			w.Fault = func(stage string) error {
				if stage == "publish-before-rename" {
					return errors.New("process stopped before publication")
				}
				return nil
			}
			for i := 0; i < 100; i++ {
				if err := w.Step(ctx, j); err != nil {
					t.Fatal(err)
				}
				var err error
				j, err = w.Queue.Get(ctx, j.Owner, j.ID)
				if err != nil {
					t.Fatal(err)
				}
				if j.Status == "failed" {
					break
				}
			}
			if j.Status != "failed" || j.Committed.Offset == 0 {
				t.Fatalf("fixture did not reach publication: %+v", j)
			}
			original, err := os.ReadFile(w.Queue.Path(j.ID, ".csv.part"))
			if err != nil {
				t.Fatal(err)
			}
			committed := j.Committed
			if err = w.Queue.Control(ctx, j.Owner, j.ID, "retry"); err != nil {
				t.Fatal(err)
			}
			j, err = w.Queue.Next(ctx, "")
			if err != nil {
				t.Fatal(err)
			}
			j.Heartbeat = time.Now().Add(-time.Hour)
			if err = w.Queue.Save(ctx, j); err != nil {
				t.Fatal(err)
			}
			var attempts []time.Time
			w.Fault = func(stage string) error {
				if stage != "checkpoint-validation-progress" {
					return nil
				}
				if err := w.heartbeat(ctx); err != nil {
					return err
				}
				got, err := w.Queue.Get(ctx, j.Owner, j.ID)
				if err != nil {
					return err
				}
				if got.Status != "recovering" || time.Since(got.Heartbeat) > time.Second {
					t.Fatalf("live recovery was presented with a stale worker heartbeat: %+v", got)
				}
				v := got.Verification
				if v == nil || v.TotalBytes != committed.Offset || v.VerifiedBytes != 0 || v.StartedAt.IsZero() {
					t.Fatalf("recovery did not restart validation from byte zero: %+v", v)
				}
				if got.Committed.Offset != committed.Offset || got.WrittenRows != committed.Rows || got.Checkpoint != committed.Unit {
					t.Fatal("verification advanced an export checkpoint")
				}
				if got.PhaseETA.State == "stable" || got.Throughput.WindowSeconds != 0 {
					t.Fatal("recovery reused generation throughput")
				}
				attempts = append(attempts, v.StartedAt)
				if len(attempts) == 1 {
					if err = w.Queue.Control(ctx, j.Owner, j.ID, "pause"); err != nil {
						return err
					}
					return context.Canceled
				}
				return nil
			}
			if err = w.Step(ctx, j); err != nil {
				t.Fatal(err)
			}
			j, err = w.Queue.Get(ctx, j.Owner, j.ID)
			if err != nil {
				t.Fatal(err)
			}
			if j.Status != "paused" || len(attempts) != 1 {
				t.Fatalf("recovery pause was lost: %+v", j)
			}
			if err = w.Queue.Control(ctx, j.Owner, j.ID, "resume"); err != nil {
				t.Fatal(err)
			}
			j, err = w.Queue.Next(ctx, "")
			if err != nil {
				t.Fatal(err)
			}
			j = finishWorker(t, w, j)
			if len(attempts) != 2 || !attempts[1].After(attempts[0]) || j.Verification != nil {
				t.Fatalf("resume trusted an old partial checksum: %+v", attempts)
			}
			published, err := os.ReadFile(w.Queue.Path(j.ID, ".csv"))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(original, published) {
				t.Fatal("recovery changed CSV rows")
			}
		})
	}
}

func TestRecoveryUsesOnlyFreshActiveWorkerMeasurements(t *testing.T) {
	w, j := workerFixture(t, "raw")
	j.Status = "recovering"
	j.Heartbeat = time.Now().Add(-time.Hour)
	if err := w.Queue.Save(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	a := Availability{Available: true, At: now, ActiveJob: j.ID, CPUCoreLimit: .03, Resources: &ResourceUsage{Scope: "worker_group", At: now, MemoryCurrentBytes: 1234}}
	check := func(want bool) {
		t.Helper()
		if err := atomicJSON(w.Queue.Dir+"/worker.json", a); err != nil {
			t.Fatal(err)
		}
		got, err := w.Queue.Get(context.Background(), j.Owner, j.ID)
		if err != nil {
			t.Fatal(err)
		}
		if (got.Resources != nil) != want {
			t.Fatalf("wrong recovery resource visibility: %+v", got)
		}
		if want && (!got.Heartbeat.Equal(now) || got.CPUCoreLimit != .03 || got.Resources.MemoryCurrentBytes != 1234) {
			t.Fatal("recovery did not use actual worker observation")
		}
		if got.Throughput.WindowSeconds != 0 || got.PhaseETA.State == "stable" {
			t.Fatal("recovery advertised generation throughput")
		}
	}
	check(true)
	a.ActiveJob = "another-task"
	check(false)
	a.ActiveJob = j.ID
	a.At = now.Add(-time.Minute)
	check(false)
}
