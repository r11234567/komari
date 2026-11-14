package historyexport

import (
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/komari-monitor/komari/pkg/metric"
)

func workerFixture(t *testing.T, mode string) (*Worker, *Job) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	dsn := filepath.Join(dir, "metrics.db")
	archive := filepath.Join(dir, "archive")
	s, e := metric.Open(ctx, metric.SQLite(dsn, metric.WithTablePrefix("metric_"), metric.WithRetiredLegacyStorage(), metric.WithSQLiteProfile(metric.SQLiteProfileDurable)))
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if e = s.UpsertMetric(ctx, metric.Definition{Name: "cpu", Unit: "percent", RetentionDays: 30}); e != nil {
		t.Fatal(e)
	}
	tel, e := s.OpenTelemetry(ctx, metric.TelemetryConfig{ArchiveDirectory: archive})
	if e != nil {
		t.Fatal(e)
	}
	defer tel.Close()
	now := time.Now().UTC().Truncate(time.Minute)
	at := now.Add(-30 * time.Minute)
	points := []metric.Point{{MetricName: "cpu", EntityID: "node", Timestamp: at.Add(2 * time.Second), Value: 20}, {MetricName: "cpu", EntityID: "node", Timestamp: at.Add(time.Second), Value: 10}, {MetricName: "cpu", EntityID: "node", Timestamp: at.Add(31 * time.Second), Value: 30}}
	if _, e = tel.Accept(ctx, metric.TelemetryBatch{Source: "node", Identity: "test", Points: points}, now); e != nil {
		t.Fatal(e)
	}
	for {
		n, e := tel.SummarizeNext(ctx)
		if e != nil {
			t.Fatal(e)
		}
		if n == 0 {
			break
		}
	}
	if _, e = tel.SealDue(ctx, now.Add(time.Hour)); e != nil {
		t.Fatal(e)
	}
	if _, e = tel.ArchiveNext(ctx); e != nil {
		t.Fatal(e)
	}
	q, e := Open(filepath.Join(dir, "exports"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { q.Close() })
	j, e := q.Create(ctx, Request{Owner: "owner", UUID: "node", NodeName: "node", Mode: mode, Category: "resource", Start: at, End: now, Metrics: []string{"cpu"}, Aggregation: metric.AggAvg, Interval: 30 * time.Second, Source: Source{DSN: dsn, Prefix: "metric_", Telemetry: metric.TelemetryConfig{ArchiveDirectory: archive}}})
	if e != nil {
		t.Fatal(e)
	}
	w := &Worker{Queue: q, Verify: func() (ResourceProof, error) { return ResourceProof{}, nil }}
	return w, j
}
func finishWorker(t *testing.T, w *Worker, j *Job) *Job {
	t.Helper()
	for i := 0; i < 100; i++ {
		if e := w.Step(context.Background(), j); e != nil {
			t.Fatal(e)
		}
		var e error
		j, e = w.Queue.Get(context.Background(), j.Owner, j.ID)
		if e != nil {
			t.Fatal(e)
		}
		if j.Status == "done" {
			return j
		}
		if j.Status == "failed" {
			t.Fatal(j.Error)
		}
	}
	t.Fatal("worker did not finish")
	return nil
}
func readCSV(t *testing.T, w *Worker, j *Job) [][]string {
	t.Helper()
	b, e := os.ReadFile(w.Queue.Path(j.ID, ".csv"))
	if e != nil {
		t.Fatal(e)
	}
	h := sha256.Sum256(b)
	if hex.EncodeToString(h[:]) != j.SHA256 {
		t.Fatal("wrong output checksum")
	}
	r := csv.NewReader(strings.NewReader(strings.TrimPrefix(string(b), "\ufeff")))
	rows, e := r.ReadAll()
	if e != nil {
		t.Fatal(e)
	}
	return rows
}
func TestWorkerThreeModesHaveDifferentSemantics(t *testing.T) {
	for _, mode := range []string{"raw", "summary", "sample30s"} {
		t.Run(mode, func(t *testing.T) {
			w, j := workerFixture(t, mode)
			j = finishWorker(t, w, j)
			rows := readCSV(t, w, j)
			if j.ReadyAt.IsZero() || j.ExpiresAt.Sub(j.ReadyAt) != TTL || j.ProgressKind != "complete" {
				t.Fatalf("invalid publication %+v", j)
			}
			switch mode {
			case "raw":
				if len(rows) != 4 || rows[1][7] != "20" || rows[2][7] != "10" {
					t.Fatalf("raw input identities/order changed: %v", rows)
				}
			case "summary":
				if len(rows) != 3 || rows[1][5] != "15" || rows[1][8] != "2" {
					t.Fatalf("summary statistics changed: %v", rows)
				}
			case "sample30s":
				if len(rows) != 3 || rows[1][2] != "10" || rows[2][2] != "30" {
					t.Fatalf("sampling was averaged or nondeterministic: %v", rows)
				}
			}
		})
	}
}
func TestWorkerRecoveryTruncatesTailAndPublishesExactlyOnce(t *testing.T) {
	for _, mode := range []string{"raw", "summary", "sample30s"} {
		t.Run(mode, func(t *testing.T) {
			for _, stage := range []string{"snapshot-before-manifest", "checkpoint-before-sync", "checkpoint-after-sync", "checkpoint-after-commit", "publish-before-rename", "publish-after-rename"} {
				t.Run(stage, func(t *testing.T) {
					w, j := workerFixture(t, mode)
					hit := false
					w.Fault = func(s string) error {
						if s == stage && !hit {
							hit = true
							return errors.New("injected crash boundary")
						}
						return nil
					}
					if e := w.Step(context.Background(), j); e != nil {
						t.Fatal(e)
					}
					if !hit {
						t.Fatal("fault not exercised")
					}
					saved, e := w.Queue.Get(context.Background(), j.Owner, j.ID)
					if e != nil {
						t.Fatal(e)
					}
					if saved.Status == "done" {
						t.Fatal("failed publication announced ready")
					}
					if _, e = os.Stat(w.Queue.Path(j.ID, ".csv.part")); e == nil {
						f, e := os.OpenFile(w.Queue.Path(j.ID, ".csv.part"), os.O_APPEND|os.O_WRONLY, 0600)
						if e != nil {
							t.Fatal(e)
						}
						f.WriteString("unconfirmed corrupt tail\n")
						f.Close()
					}
					w.Fault = nil
					saved.Status = "running"
					if e = w.Queue.Save(context.Background(), saved); e != nil {
						t.Fatal(e)
					}
					saved = finishWorker(t, w, saved)
					rows := readCSV(t, w, saved)
					want := 4
					if mode != "raw" {
						want = 3
					}
					if len(rows) != want {
						t.Fatal("duplicate or missing rows", rows)
					}
				})
			}
		})
	}
}
func TestWorkerPauseCancelAndMissingQuotaFailClosed(t *testing.T) {
	w, j := workerFixture(t, "raw")
	if e := w.Queue.Control(context.Background(), j.Owner, j.ID, "pause"); e != nil {
		t.Fatal(e)
	}
	if e := w.Step(context.Background(), j); e != nil {
		t.Fatal(e)
	}
	got, e := w.Queue.Get(context.Background(), j.Owner, j.ID)
	if e != nil {
		t.Fatal(e)
	}
	if got.Status == "done" {
		t.Fatal("paused task published")
	}
	w.Verify = func() (ResourceProof, error) { return ResourceProof{}, errors.New("quota missing") }
	if e = w.Step(context.Background(), got); e == nil {
		t.Fatal("work continued without hard cap")
	}
}
