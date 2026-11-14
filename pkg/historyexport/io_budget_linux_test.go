//go:build linux

package historyexport

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestIOBudgetPacesCachedReadsAtEveryBatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cached-input")
	if err := os.WriteFile(path, make([]byte, 256<<10), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b := &ioBudget{rate: 1 << 20}
	ctx := context.Background()
	if err = b.wait(ctx); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 16<<10)
	started := time.Now()
	for i := 0; i < 16; i++ {
		if _, err = io.ReadFull(f, buf); err != nil {
			t.Fatal(err)
		}
		if err = b.wait(ctx); err != nil {
			t.Fatal(err)
		}
	}
	// This is a real page-cache read. The old 100 ms shortcut returned before
	// sampling any of the 256 KiB, silently allowing the whole burst.
	if elapsed := time.Since(started); elapsed < 240*time.Millisecond {
		t.Fatalf("cached batches escaped the 1 MiB/s budget: %s", elapsed)
	}
	if waiting, duration := b.observation(); waiting || duration <= 0 {
		t.Fatalf("completed pacing lacks measured wait: %t %s", waiting, duration)
	}
}

func TestIOBudgetReportsCurrentWaitAndCancelsDebt(t *testing.T) {
	b := &ioBudget{rate: 64 << 10}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.wait(ctx); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "budget-debt")
	if err := os.WriteFile(path, make([]byte, 256<<10), 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- b.wait(ctx) }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		waiting, duration := b.observation()
		if waiting && duration > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("I/O wait was not visible while it was in progress")
		}
		time.Sleep(time.Millisecond)
	}
	_, before := b.observation()
	time.Sleep(20 * time.Millisecond)
	_, after := b.observation()
	if after <= before {
		t.Fatalf("live wait duration froze: before=%s after=%s", before, after)
	}
	at := time.Now()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("unexpected wait result: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation waited for the entire I/O debt")
	}
	if waiting, duration := b.observation(); waiting || duration < after {
		t.Fatalf("cancelled wait lost its measurement: %t %s", waiting, duration)
	}
	if time.Since(at) > time.Second {
		t.Fatal("cancellation was not prompt")
	}
}
