package historyexport

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mattn/go-sqlite3"
)

func TestRecoveryRejectsLostOrCorruptCommittedOutput(t *testing.T) {
	for _, mutation := range []string{"missing", "short", "corrupt", "unconfirmed-tail", "valid"} {
		t.Run(mutation, func(t *testing.T) {
			w, j := workerFixture(t, "raw")
			w.Fault = func(stage string) error {
				if stage == "checkpoint-after-commit" {
					return errors.New("simulated process exit")
				}
				return nil
			}
			if err := w.Step(context.Background(), j); err != nil {
				t.Fatal(err)
			}
			j, _ = w.Queue.Get(context.Background(), j.Owner, j.ID)
			if j.Committed.Offset == 0 {
				t.Fatal("no committed fixture prefix")
			}
			p := w.Queue.Path(j.ID, ".csv.part")
			data, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			switch mutation {
			case "missing":
				err = os.Remove(p)
			case "short":
				err = os.Truncate(p, j.Committed.Offset-1)
			case "corrupt":
				data[0] ^= 1
				err = os.WriteFile(p, data, 0600)
			case "unconfirmed-tail":
				err = os.WriteFile(p, append(data, []byte("uncommitted garbage")...), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			j.Status = "running"
			if err = w.Queue.Save(context.Background(), j); err != nil {
				t.Fatal(err)
			}
			if err = w.Queue.Recover(context.Background()); err != nil {
				t.Fatal(err)
			}
			j, _ = w.Queue.Get(context.Background(), j.Owner, j.ID)
			if j.Status != "recovering" {
				t.Fatal(j.Status)
			}
			w.Fault = nil
			if mutation == "valid" || mutation == "unconfirmed-tail" {
				j = finishWorker(t, w, j)
				if len(readCSV(t, w, j)) != 4 {
					t.Fatal("recovery lost or duplicated rows")
				}
			} else {
				if err = w.Step(context.Background(), j); err != nil {
					t.Fatal(err)
				}
				j, _ = w.Queue.Get(context.Background(), j.Owner, j.ID)
				if j.Status != "failed" || !strings.Contains(j.Error, "checkpoint") {
					t.Fatalf("corruption accepted: %+v", j)
				}
				if _, err = os.Stat(w.Queue.Path(j.ID, ".csv")); !os.IsNotExist(err) {
					t.Fatal("corrupt output published")
				}
			}
		})
	}
}

func TestTransientBusyRetriesAreDelayedBoundedAndCancellable(t *testing.T) {
	w, j := workerFixture(t, "raw")
	w.Fault = func(stage string) error {
		if stage == "snapshot-before-manifest" {
			return sqlite3.Error{Code: sqlite3.ErrBusy}
		}
		return nil
	}
	for attempt := 1; attempt <= 4; attempt++ {
		if err := w.Step(context.Background(), j); err != nil {
			t.Fatal(err)
		}
		j, _ = w.Queue.Get(context.Background(), j.Owner, j.ID)
		if attempt == 4 {
			if j.Status != "failed" {
				t.Fatal("unbounded retries", j.Status)
			}
			break
		}
		if j.Status != "retry_wait" || j.AutomaticRetries != attempt || !j.RetryAt.After(time.Now()) {
			t.Fatalf("retry missing: %+v", j)
		}
		if next, err := w.Queue.Next(context.Background(), ""); err != nil || next != nil {
			t.Fatal("retry ran before deadline", err)
		}
		j.RetryAt = time.Now().Add(-time.Second)
		if err := w.Queue.Save(context.Background(), j); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Queue.Control(context.Background(), j.Owner, j.ID, "retry"); err != nil {
		t.Fatal(err)
	}
	j, _ = w.Queue.Get(context.Background(), j.Owner, j.ID)
	if err := w.Step(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	if err := w.Queue.Control(context.Background(), j.Owner, j.ID, "cancel"); err != nil {
		t.Fatal(err)
	}
	j, err := w.Queue.Next(context.Background(), "")
	if err != nil || j == nil {
		t.Fatal("delayed retry blocked cancellation", err)
	}
	if err = w.Step(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	j, _ = w.Queue.Get(context.Background(), j.Owner, j.ID)
	if j.Status != "cancelled" {
		t.Fatal(j.Status)
	}
}

func TestPrunePagesMoreThanOneBatch(t *testing.T) {
	w, fixture := workerFixture(t, "raw")
	for i := 0; i < 133; i++ {
		j := *fixture
		j.ID = fmt.Sprintf("%024x", i+1)
		j.Status = "failed"
		b, err := encodeJob(&j)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = w.Queue.db.Exec("INSERT INTO jobs(id,owner,dedupe,created,status,body) VALUES(?,?,?,?,?,?)", j.ID, j.Owner, j.ID, 0, j.Status, b); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Queue.Prune(context.Background(), time.Now().Add(8*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := w.Queue.db.QueryRow("SELECT COUNT(*) FROM jobs").Scan(&n); err != nil || n != 1 {
		t.Fatal(n, err)
	}
}
