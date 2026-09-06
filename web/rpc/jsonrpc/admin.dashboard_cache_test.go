package jsonrpc

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// A dashboard module load is shared by every caller asking for it. The caller
// that happens to start it may navigate away or time out, and when it does the
// load must keep running: cancelling it would fail every other caller waiting
// on the same result and discard work that was about to fill the cache, leaving
// the retries to contend for the metric store's single heavy-read slot again.
func TestDashboardModuleLoadSurvivesTheCallerThatStartedIt(t *testing.T) {
	var cache dashboardModuleCache[int]
	now := time.Now()

	started := make(chan struct{})
	release := make(chan struct{})
	loadDone := make(chan error, 1)
	var observedErr error

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, observedErr = cache.get(leaderCtx, now, "k", time.Minute, func(loadCtx context.Context) (int, error) {
			close(started)
			<-release
			loadDone <- loadCtx.Err()
			return 42, nil
		})
	}()

	<-started
	cancelLeader()
	// Give the cancellation a chance to propagate before the load finishes.
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()

	if !errors.Is(observedErr, context.Canceled) {
		t.Fatalf("leader error = %v, want context.Canceled", observedErr)
	}
	select {
	case loadCtxErr := <-loadDone:
		if loadCtxErr != nil {
			t.Fatalf("shared load saw ctx error %v, want it to outlive the caller", loadCtxErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shared load never finished")
	}
	// The abandoned load still populated the cache, so the retry is free. The
	// store happens just after the load returns, so allow it a moment to land.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if value, ok := cache.current(now, "k", time.Minute); ok {
			if value != 42 {
				t.Fatalf("cached value = %d, want 42", value)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the abandoned load never stored its result")
		}
		time.Sleep(5 * time.Millisecond)
	}

	var reloaded bool
	value, err := cache.get(context.Background(), now, "k", time.Minute,
		func(context.Context) (int, error) {
			reloaded = true
			return 0, nil
		})
	if err != nil || value != 42 {
		t.Fatalf("cached value = %d, err = %v, want 42, nil", value, err)
	}
	if reloaded {
		t.Fatal("the retry re-ran the scan instead of using the cached result")
	}
}

func TestDashboardModuleCacheSharesOneLoadAcrossCallers(t *testing.T) {
	var cache dashboardModuleCache[int]
	now := time.Now()
	release := make(chan struct{})
	var loads int
	var mu sync.Mutex

	var wg sync.WaitGroup
	results := make([]int, 8)
	errs := make([]error, 8)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			value, err := cache.get(context.Background(), now, "shared", time.Minute,
				func(context.Context) (int, error) {
					mu.Lock()
					loads++
					mu.Unlock()
					<-release
					return 7, nil
				})
			errs[i] = err
			results[i] = value
		}(i)
	}
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d: %v", i, err)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if loads != 1 {
		t.Fatalf("ran %d loads for one key, want exactly 1", loads)
	}
	for i, value := range results {
		if value != 7 {
			t.Fatalf("caller %d got %d, want 7", i, value)
		}
	}
}

func TestDashboardModuleLoadContextKeepsValuesAndBounds(t *testing.T) {
	type traceKey struct{}
	parent, cancel := context.WithCancel(context.WithValue(context.Background(), traceKey{}, "abc"))
	loadCtx, release := dashboardModuleLoadContext(parent)
	defer release()

	cancel()
	if loadCtx.Err() != nil {
		t.Fatal("cancelling the request must not cancel the shared load")
	}
	if loadCtx.Value(traceKey{}) != "abc" {
		t.Fatal("request values must survive so authorization and tracing still work")
	}
	deadline, ok := loadCtx.Deadline()
	if !ok {
		t.Fatal("an abandoned load must not run unbounded")
	}
	if remaining := time.Until(deadline); remaining > dashboardModuleLoadBudget+time.Second {
		t.Fatalf("load budget = %s, want at most %s", remaining, dashboardModuleLoadBudget)
	}
}

func TestDashboardModuleCacheDoesNotCacheFailures(t *testing.T) {
	var cache dashboardModuleCache[int]
	now := time.Now()
	sentinel := errors.New("load failed")

	if _, err := cache.get(context.Background(), now, "k", time.Minute,
		func(context.Context) (int, error) { return 0, sentinel }); !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want the load error", err)
	}
	// A failed load must not poison the cache; the next caller retries.
	value, err := cache.get(context.Background(), now, "k", time.Minute,
		func(context.Context) (int, error) { return 5, nil })
	if err != nil || value != 5 {
		t.Fatalf("retry after failure = %d, %v, want 5, nil", value, err)
	}
}
