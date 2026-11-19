package historyexport

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

// Manual retry must validate exactly the same durable bytes as process
// recovery. Restoring a saved hash state alone cannot detect a damaged prefix.
func TestManualRetryValidatesCommittedPrefix(t *testing.T) {
	for _, mode := range []string{"raw", "summary", "sample30s"} {
		t.Run(mode, func(t *testing.T) {
			for _, corrupt := range []bool{false, true} {
				t.Run(map[bool]string{false: "valid", true: "same-size-corruption"}[corrupt], func(t *testing.T) {
					ctx := context.Background()
					w, j := workerFixture(t, mode)
					w.Fault = func(stage string) error {
						if stage == "checkpoint-after-commit" {
							return errors.New("interrupted after durable checkpoint")
						}
						return nil
					}
					if err := w.Step(ctx, j); err != nil {
						t.Fatal(err)
					}
					j, _ = w.Queue.Get(ctx, j.Owner, j.ID)
					if j.Status != "failed" || j.Committed.Offset == 0 {
						t.Fatalf("missing failed checkpoint: %+v", j)
					}
					if corrupt {
						file := w.Queue.Path(j.ID, ".csv.part")
						b, err := os.ReadFile(file)
						if err != nil {
							t.Fatal(err)
						}
						b[0] ^= 1
						if err = os.WriteFile(file, b, 0600); err != nil {
							t.Fatal(err)
						}
					}
					if err := w.Queue.Control(ctx, j.Owner, j.ID, "retry"); err != nil {
						t.Fatal(err)
					}
					j, _ = w.Queue.Next(ctx, "")
					w.Fault = nil
					if !corrupt {
						j = finishWorker(t, w, j)
						want := 3
						if mode == "raw" {
							want = 4
						}
						if len(readCSV(t, w, j)) != want {
							t.Fatal("valid retry lost observations")
						}
						return
					}
					if err := w.Step(ctx, j); err != nil {
						t.Fatal(err)
					}
					j, _ = w.Queue.Get(ctx, j.Owner, j.ID)
					if j.Status != "failed" || !strings.Contains(j.Error, "checksum") {
						t.Fatalf("manual retry accepted corrupt committed output: status=%s error=%q", j.Status, j.Error)
					}
					if _, err := os.Stat(w.Queue.Path(j.ID, ".csv")); !os.IsNotExist(err) {
						t.Fatal("corrupt output published", err)
					}
				})
			}
		})
	}
}
