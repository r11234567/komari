package qq

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestConsumeStateIsAtomicUnderConcurrency(t *testing.T) {
	provider := &QQ{}
	if err := provider.Init(); err != nil {
		t.Fatal(err)
	}
	defer provider.Destroy()

	const state = "concurrent-state-token"
	if !provider.stateStore.Issue(state) {
		t.Fatal("failed to issue state")
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
			if provider.consumeState(state) {
				winners.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()

	if got := winners.Load(); got != 1 {
		t.Fatalf("consumeState winners = %d, want exactly 1", got)
	}
	if provider.consumeState(state) {
		t.Fatal("reused state unexpectedly succeeded")
	}
}
