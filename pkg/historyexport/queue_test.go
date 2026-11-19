package historyexport

import (
	"context"
	"errors"
	"github.com/komari-monitor/komari/pkg/metric"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func requestFixture(t *testing.T, owner string) Request {
	t.Helper()
	return Request{Owner: owner, UUID: "node", Category: "resource", Mode: "raw", Start: time.Now().Add(-time.Hour), End: time.Now().Add(-time.Minute), Metrics: []string{"cpu.usage"}, Source: Source{DSN: filepath.Join(t.TempDir(), "metrics.db"), Telemetry: metric.TelemetryConfig{ArchiveDirectory: filepath.Join(t.TempDir(), "archive")}}}
}

func TestQueuePrivateDirectoryAndCancellationPriority(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "queue")
	if e := os.Mkdir(dir, 0755); e != nil {
		t.Fatal(e)
	}
	q, e := Open(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer q.Close()
	if runtime.GOOS != "windows" {
		info, e := os.Stat(dir)
		if e != nil || info.Mode().Perm() != 0700 {
			t.Fatal("private exports exposed", info, e)
		}
	}
	old, e := q.Create(ctx, requestFixture(t, "old"))
	if e != nil {
		t.Fatal(e)
	}
	later, e := q.Create(ctx, requestFixture(t, "later"))
	if e != nil {
		t.Fatal(e)
	}
	if e = q.Control(ctx, later.Owner, later.ID, "cancel"); e != nil {
		t.Fatal(e)
	}
	next, e := q.Next(ctx, "later")
	if e != nil || next.ID != later.ID || next.ID == old.ID {
		t.Fatal("cancellation starved behind active export", next, e)
	}
}

func TestQueueRestartTTLAndControl(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	q, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	j, err := q.Create(ctx, requestFixture(t, "a"))
	if err != nil {
		t.Fatal(err)
	}
	j.Status = "running"
	j.OutputBytes = 7
	j.Checkpoint = 3
	j.CreatedAt = time.Now().Add(-72 * time.Hour)
	if err = q.Save(ctx, j); err != nil {
		t.Fatal(err)
	}
	q.Close()
	q, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	if err = q.Prune(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	got, err := q.Get(ctx, "a", j.ID)
	if err != nil || got.OutputBytes != 7 || got.Checkpoint != 3 {
		t.Fatalf("lost durable checkpoint: %+v %v", got, err)
	}
	if err = q.Control(ctx, "b", j.ID, "cancel"); !errors.Is(err, ErrNotFound) {
		t.Fatal("cross-owner control", err)
	}
	if _, err = q.Get(ctx, "b", j.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("cross-owner read", err)
	}
	if err = q.Control(ctx, "a", j.ID, "pause"); err != nil {
		t.Fatal(err)
	}
	if next, e := q.Next(ctx, ""); e != nil || next != nil {
		t.Fatal("paused task scheduled", next, e)
	}
	if err = q.Control(ctx, "a", j.ID, "resume"); err != nil {
		t.Fatal(err)
	}
	if next, e := q.Next(ctx, ""); e != nil || next == nil {
		t.Fatal("resumable task lost", e)
	}
	got.Status = "done"
	got.ReadyAt = time.Now()
	got.ExpiresAt = got.ReadyAt.Add(TTL)
	if err = q.Save(ctx, got); err != nil {
		t.Fatal(err)
	}
	if err = q.Prune(ctx, got.ReadyAt.Add(47*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err = q.Get(ctx, "a", j.ID); err != nil {
		t.Fatal("TTL began before ready", err)
	}
	if err = q.Prune(ctx, got.ReadyAt.Add(49*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if expired, e := q.Get(ctx, "a", j.ID); e != nil || expired.Status != "expired" {
		t.Fatal("expiry metadata missing", expired, e)
	}
	if err = q.Prune(ctx, got.ExpiresAt.Add(8*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err = q.Get(ctx, "a", j.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("expired metadata was retained indefinitely", err)
	}
}
func TestQueueDedupeFairnessAndBounds(t *testing.T) {
	ctx := context.Background()
	q, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	r := requestFixture(t, "a")
	one, err := q.Create(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	same, err := q.Create(ctx, r)
	if err != nil || same.ID != one.ID {
		t.Fatal("duplicate task", err)
	}
	r.End = r.End.Add(-time.Second)
	if _, err = q.Create(ctx, r); err != nil {
		t.Fatal(err)
	}
	r.End = r.End.Add(-time.Second)
	if _, err = q.Create(ctx, r); !errors.Is(err, ErrQueueFull) {
		t.Fatal("per-owner bound", err)
	}
	b, err := q.Create(ctx, requestFixture(t, "b"))
	if err != nil {
		t.Fatal(err)
	}
	next, err := q.Next(ctx, "a")
	if err != nil || next.ID != b.ID {
		t.Fatal("account fairness", err)
	}
	r.Mode = "pretend-raw"
	if _, err = q.Create(ctx, r); err == nil {
		t.Fatal("accepted ambiguous mode")
	}
}
