package historyexport

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

func pauseAtPublication(t *testing.T, mode string) (*Worker, *Job) {
	t.Helper()
	w, j := workerFixture(t, mode)
	w.Fault = func(stage string) error {
		if stage == "publish-before-rename" {
			return w.Queue.Control(context.Background(), j.Owner, j.ID, "pause")
		}
		return nil
	}
	for i := 0; i < 100; i++ {
		if err := w.Step(context.Background(), j); err != nil {
			t.Fatal(err)
		}
		var err error
		j, err = w.Queue.Get(context.Background(), j.Owner, j.ID)
		if err != nil {
			t.Fatal(err)
		}
		if j.Status == "paused" {
			if j.Committed.Offset == 0 || !w.canContinueCheckpoint(j) {
				t.Fatal("live writer did not retain its verified checkpoint")
			}
			return w, j
		}
	}
	t.Fatal("publication pause not reached")
	return nil, nil
}

func TestLivePauseResumesWithoutReadingPrefix(t *testing.T) {
	for _, mode := range []string{"raw", "summary", "sample30s"} {
		t.Run(mode, func(t *testing.T) {
			w, j := pauseAtPublication(t, mode)
			w.Fault = func(stage string) error {
				if stage == "checkpoint-validation-progress" {
					t.Fatal("ordinary pause restarted full-prefix recovery")
				}
				return nil
			}
			if err := w.Queue.Control(context.Background(), j.Owner, j.ID, "resume"); err != nil {
				t.Fatal(err)
			}
			var err error
			j, err = w.Queue.Next(context.Background(), "")
			if err != nil {
				t.Fatal(err)
			}
			j = finishWorker(t, w, j)
			want := 3
			if mode == "raw" {
				want = 4
			}
			if len(readCSV(t, w, j)) != want {
				t.Fatal("lost or duplicate output")
			}
			if _, ok := w.continuations[j.ID]; ok {
				t.Fatal("completed job retained a continuation")
			}
		})
	}
}

func TestLiveContinuationRejectsChangedFilesAndRestarts(t *testing.T) {
	for _, mutation := range []string{"restart", "rewrite", "truncate", "replace", "tail", "manual-retry"} {
		t.Run(mutation, func(t *testing.T) {
			w, j := pauseAtPublication(t, "raw")
			ctx := context.Background()
			path := w.Queue.Path(j.ID, ".csv.part")
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			// Reading can change atime; restore the writer proof before mutation.
			if err = w.rememberCheckpoint(j); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			corrupt := false
			switch mutation {
			case "restart":
				w = &Worker{Queue: w.Queue, Verify: w.Verify}
			case "rewrite":
				b[0] ^= 1
				if err = os.WriteFile(path, b, 0600); err != nil {
					t.Fatal(err)
				}
				if err = os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
					t.Fatal(err)
				}
				corrupt = true
			case "truncate":
				if err = os.Truncate(path, int64(len(b)-1)); err != nil {
					t.Fatal(err)
				}
				corrupt = true
			case "replace":
				if err = os.Rename(path, path+".old"); err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(path, b, 0600); err != nil {
					t.Fatal(err)
				}
			case "tail":
				f, e := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
				if e != nil {
					t.Fatal(e)
				}
				_, err = f.WriteString("uncommitted tail")
				f.Close()
				if err != nil {
					t.Fatal(err)
				}
			case "manual-retry":
				// Even an unchanged live proof cannot bypass an explicit retry.
				j.Status = "failed"
				if err = w.Queue.Save(ctx, j); err != nil {
					t.Fatal(err)
				}
			}
			if mutation != "manual-retry" && w.canContinueCheckpoint(j) {
				t.Fatal("changed file retained trusted continuation")
			}
			action := "resume"
			if mutation == "manual-retry" {
				action = "retry"
			}
			if err = w.Queue.Control(ctx, j.Owner, j.ID, action); err != nil {
				t.Fatal(err)
			}
			j, err = w.Queue.Next(ctx, "")
			if err != nil {
				t.Fatal(err)
			}
			validated := false
			w.Fault = func(stage string) error {
				if stage == "checkpoint-validation-progress" {
					validated = true
				}
				return nil
			}
			if corrupt {
				if err = w.Step(ctx, j); err != nil {
					t.Fatal(err)
				}
				j, err = w.Queue.Get(ctx, j.Owner, j.ID)
				if err != nil {
					t.Fatal(err)
				}
				if j.Status != "failed" || !(strings.Contains(j.Error, "checksum") || strings.Contains(j.Error, "size mismatch")) {
					t.Fatal("corrupt prefix accepted", j.Status, j.Error)
				}
				return
			}
			j = finishWorker(t, w, j)
			if !validated {
				t.Fatal("changed/retried prefix was not verified")
			}
			if len(readCSV(t, w, j)) != 4 {
				t.Fatal(errors.New("recovery changed rows"))
			}
		})
	}
}
