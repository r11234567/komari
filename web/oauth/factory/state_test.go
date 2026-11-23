package factory

import (
	"bytes"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestStateStoreSingleUseConcurrencyAndBounds(t *testing.T) {
	now := time.Unix(1700000000, 0)
	store := NewStateStore(5*time.Minute, 2)
	store.now = func() time.Time { return now }

	if !store.Issue("s1") {
		t.Fatal("failed to issue first state")
	}
	if !store.Issue("s2") {
		t.Fatal("failed to issue second state")
	}
	if store.Issue("s3") {
		t.Fatal("state store exceeded maxEntries capacity")
	}

	const workers = 16
	var winners atomic.Int32
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			<-start
			if store.Consume("s1") {
				winners.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if got := winners.Load(); got != 1 {
		t.Fatalf("concurrent Consume winners = %d, want 1", got)
	}
	if store.Consume("s1") {
		t.Fatal("replayed state unexpectedly succeeded")
	}

	// Advancing past TTL expires s2 and frees capacity for s3.
	now = now.Add(6 * time.Minute)
	if !store.Issue("s3") {
		t.Fatal("expected expired entry to be pruned before capacity check")
	}
	if store.Consume("s2") {
		t.Fatal("expired state unexpectedly succeeded")
	}
}

func TestReadBoundedBodyRejectsOversizedPayload(t *testing.T) {
	within, err := ReadBoundedBody(strings.NewReader("0123456789abcdef"), 16)
	if err != nil {
		t.Fatalf("ReadBoundedBody within limit failed: %v", err)
	}
	if string(within) != "0123456789abcdef" {
		t.Fatalf("got %q, want 16 bytes", within)
	}

	if _, err := ReadBoundedBody(bytes.NewReader(bytes.Repeat([]byte("a"), 17)), 16); err == nil {
		t.Fatal("expected ReadBoundedBody to reject payload exceeding limit")
	}
}
