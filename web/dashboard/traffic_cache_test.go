package dashboard

import (
	"errors"
	"testing"
	"time"
)

func resetTrafficTrendCache(t *testing.T) {
	t.Helper()
	clear := func() {
		trafficTrendCache.Lock()
		trafficTrendCache.items = nil
		trafficTrendCache.Unlock()
	}
	clear()
	t.Cleanup(clear)
}

func TestLoadTrafficTrendReportsBadRequestsDistinctly(t *testing.T) {
	resetTrafficTrendCache(t)
	// A caller-supplied interval outside the dashboard range is the caller's
	// fault and must be separable from a store that is merely busy, otherwise the
	// transport cannot decide whether retrying is pointless or worth doing.
	_, err := LoadTrafficTrend(t.Context(), nil, time.Now(), DefaultTrafficTrendWindow, time.Minute)
	if !errors.Is(err, ErrTrafficTrendBadRequest) {
		t.Fatalf("interval error = %v, want ErrTrafficTrendBadRequest", err)
	}
	_, err = LoadTrafficTrend(t.Context(), nil, time.Now(), time.Duration(maxTrafficTrendBuckets+2)*MinTrafficTrendInterval, MinTrafficTrendInterval)
	if !errors.Is(err, ErrTrafficTrendBadRequest) {
		t.Fatalf("bucket count error = %v, want ErrTrafficTrendBadRequest", err)
	}
}

// Panels on one dashboard, and separate browser tabs, request the same trend
// within milliseconds of each other. Their "now" differs slightly, so the key
// must quantize it or every panel would run its own full ledger scan against
// the store's single heavy-read slot.
func TestTrafficTrendKeySharesNearbyRequests(t *testing.T) {
	base := time.Date(2026, 9, 5, 14, 10, 3, 0, time.UTC)
	clients := []string{"b", "a"}
	key := trafficTrendKey(clients, base, DefaultTrafficTrendWindow, DefaultTrafficTrendInterval)

	if other := trafficTrendKey(clients, base.Add(900*time.Millisecond), DefaultTrafficTrendWindow, DefaultTrafficTrendInterval); other != key {
		t.Fatal("requests milliseconds apart must share one scan")
	}
	// Client order comes from a map iteration upstream and carries no meaning.
	if other := trafficTrendKey([]string{"a", "b"}, base, DefaultTrafficTrendWindow, DefaultTrafficTrendInterval); other != key {
		t.Fatal("client order must not change the key")
	}
	if other := trafficTrendKey(clients, base.Add(trafficTrendKeyQuantum), DefaultTrafficTrendWindow, DefaultTrafficTrendInterval); other == key {
		t.Fatal("a request a full quantum later must not reuse the cached scan")
	}
	if other := trafficTrendKey(clients, base, 12*time.Hour, DefaultTrafficTrendInterval); other == key {
		t.Fatal("a different window must not share a key")
	}
	if other := trafficTrendKey([]string{"a"}, base, DefaultTrafficTrendWindow, DefaultTrafficTrendInterval); other == key {
		t.Fatal("a different client set must not share a key")
	}
}

func TestTrafficTrendCacheExpiresAndStaysBounded(t *testing.T) {
	resetTrafficTrendCache(t)
	now := time.Now()
	buckets := []TrafficTrendBucket{{Start: now, Up: 1, Down: 2}}
	storeTrafficTrend("k", buckets, now)

	if got, ok := cachedTrafficTrend("k", now.Add(trafficTrendCacheTTL-time.Millisecond)); !ok || len(got) != 1 {
		t.Fatal("entry must be served inside its TTL")
	}
	if _, ok := cachedTrafficTrend("k", now.Add(trafficTrendCacheTTL)); ok {
		t.Fatal("entry must expire at its TTL")
	}
	if _, ok := cachedTrafficTrend("absent", now); ok {
		t.Fatal("an unknown key must miss")
	}

	for i := 0; i < trafficTrendCacheLimit+8; i++ {
		storeTrafficTrend(string(rune('a'+i%26))+string(rune('a'+i/26)), buckets, now)
	}
	trafficTrendCache.Lock()
	size := len(trafficTrendCache.items)
	trafficTrendCache.Unlock()
	if size > trafficTrendCacheLimit {
		t.Fatalf("cache holds %d entries, want at most %d", size, trafficTrendCacheLimit)
	}
}

func TestLoadTrafficTrendServesCacheWithoutTouchingStore(t *testing.T) {
	resetTrafficTrendCache(t)
	// No metric store is configured in this test binary, so a real scan fails.
	// Priming the cache and getting a hit proves the cached path returns before
	// reaching the store at all.
	now := time.Date(2026, 9, 5, 14, 10, 0, 0, time.UTC)
	want := []TrafficTrendBucket{{Start: now, Up: 7, Down: 9}}
	key := trafficTrendKey(nil, now, DefaultTrafficTrendWindow, DefaultTrafficTrendInterval)
	storeTrafficTrend(key, want, time.Now())

	got, err := LoadTrafficTrend(t.Context(), nil, now, DefaultTrafficTrendWindow, DefaultTrafficTrendInterval)
	if err != nil {
		t.Fatalf("cached load: %v", err)
	}
	if len(got) != 1 || got[0].Up != 7 || got[0].Down != 9 {
		t.Fatalf("cached load returned %+v, want the cached buckets", got)
	}
}
