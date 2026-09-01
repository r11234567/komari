package connectapi

import (
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	metricsv1 "github.com/r11234567/komari-proto/gen/go/komari/metrics/v1"
)

func resetMetricQueryCache(t *testing.T) {
	t.Helper()
	connectMetricQueryCache.Lock()
	connectMetricQueryCache.items = nil
	connectMetricQueryCache.Unlock()
	t.Cleanup(func() {
		connectMetricQueryCache.Lock()
		connectMetricQueryCache.items = nil
		connectMetricQueryCache.Unlock()
	})
}

// A cached dashboard read is handed to every caller that asks for the same
// window inside the TTL. Sharing one connect.Response would share its header
// and trailer maps, so a single interceptor writing a header would turn a
// popular query into a concurrent map write. Only the message may be shared.
func TestCachedMetricQueryGivesEachCallerItsOwnResponse(t *testing.T) {
	resetMetricQueryCache(t)
	now := time.Now()
	message := &metricsv1.QueryMetricsResponse{}
	storeMetricQueryMessage("window", message, now)

	first, ok := cachedMetricQueryMessage("window", now)
	if !ok {
		t.Fatal("a freshly stored entry must be readable")
	}
	second, ok := cachedMetricQueryMessage("window", now)
	if !ok {
		t.Fatal("a cached entry must stay readable within its TTL")
	}
	if first != message || second != message {
		t.Fatal("callers must observe the cached message itself")
	}

	firstResponse := connect.NewResponse(first)
	secondResponse := connect.NewResponse(second)
	if firstResponse == secondResponse {
		t.Fatal("callers must not share one connect.Response")
	}
	firstResponse.Header().Set("X-Trace", "first")
	if got := secondResponse.Header().Get("X-Trace"); got != "" {
		t.Fatalf("header written by one caller leaked to another: %q", got)
	}
}

func TestCachedMetricQueryExpiresAtTTL(t *testing.T) {
	resetMetricQueryCache(t)
	now := time.Now()
	storeMetricQueryMessage("window", &metricsv1.QueryMetricsResponse{}, now)

	if _, ok := cachedMetricQueryMessage("window", now.Add(connectMetricCacheTTL-time.Millisecond)); !ok {
		t.Fatal("entry must survive until its TTL")
	}
	if _, ok := cachedMetricQueryMessage("window", now.Add(connectMetricCacheTTL)); ok {
		t.Fatal("entry must expire once the TTL has elapsed")
	}
	if _, ok := cachedMetricQueryMessage("other", now); ok {
		t.Fatal("an unknown key must miss")
	}
}

func TestMetricQueryCacheStaysBounded(t *testing.T) {
	resetMetricQueryCache(t)
	now := time.Now()
	// Entries that are still live cannot be evicted one by one, so the map is
	// dropped wholesale rather than growing without limit.
	for i := 0; i < connectMetricQueryCacheLimit+16; i++ {
		storeMetricQueryMessage(string(rune(i))+"-live", &metricsv1.QueryMetricsResponse{}, now)
	}
	connectMetricQueryCache.Lock()
	size := len(connectMetricQueryCache.items)
	connectMetricQueryCache.Unlock()
	if size > connectMetricQueryCacheLimit {
		t.Fatalf("cache holds %d entries, want at most %d", size, connectMetricQueryCacheLimit)
	}
}

func TestMetricQueryCacheConcurrentAccess(t *testing.T) {
	resetMetricQueryCache(t)
	now := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := string(rune('a' + i%4))
			storeMetricQueryMessage(key, &metricsv1.QueryMetricsResponse{}, now)
			if message, ok := cachedMetricQueryMessage(key, now); ok {
				connect.NewResponse(message).Header().Set("X-Caller", key)
			}
		}(i)
	}
	wg.Wait()
}
