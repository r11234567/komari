package connectapi

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/komari-monitor/komari/pkg/rpc"
	metricsv1 "github.com/r11234567/komari-proto/gen/go/komari/metrics/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestMetricQueryCachePreservesExactWindow(t *testing.T) {
	ctx := context.Background()
	start := time.Date(2026, 11, 15, 3, 25, 10, 271000000, time.UTC)
	end := start.Add(15*time.Minute - 5*time.Second)
	request := func(start, end time.Time) *metricsv1.QueryMetricsRequest {
		return &metricsv1.QueryMetricsRequest{StartTime: timestamppb.New(start), EndTime: timestamppb.New(end), Metrics: []string{"cpu.usage"}}
	}
	response := func(q *metricsv1.QueryMetricsRequest) *metricsv1.QueryMetricsResponse {
		return &metricsv1.QueryMetricsResponse{Series: []*metricsv1.MetricsSeries{{CoverageStart: q.StartTime, CoverageEnd: q.EndTime}}}
	}
	for _, tc := range []struct {
		name       string
		start, end time.Duration
	}{
		{"identical", 0, 0},
		{"start_nanosecond", time.Nanosecond, 0},
		{"end_nanosecond", 0, time.Nanosecond},
		{"same_five_second_bins", 4 * time.Second, 40 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetMetricQueryCache(t)
			base := request(start, end)
			storeMetricQueryMessage(connectMetricQueryKey(ctx, base), response(base), time.Now())
			q := request(start.Add(tc.start), end.Add(tc.end))
			loaded := false
			got, err := cachedMetricRead(ctx, connectMetricQueryKey(ctx, q), func() (proto.Message, error) {
				loaded = true
				return response(q), nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if loaded != (tc.start != 0 || tc.end != 0) {
				t.Fatal("different windows must load independently; identical windows must reuse the cache", loaded)
			}
			series := got.(*metricsv1.QueryMetricsResponse).Series[0]
			if !series.CoverageStart.AsTime().Equal(q.StartTime.AsTime()) || !series.CoverageEnd.AsTime().Equal(q.EndTime.AsTime()) {
				t.Fatal("cached response belongs to a different requested population", series)
			}
		})
	}
}

func TestConnectMetricQueryCacheSeparatesScopedTokens(t *testing.T) {
	key := func(id string) string {
		p := &rpc.Principal{Type: rpc.PrincipalAPIKey, IsAPIKey: true, APIToken: &rpc.APITokenPolicy{ID: id, AgentIDs: []string{id}, ExpiresAt: time.Now().Add(time.Hour)}}
		ctx := rpc.NewContextWithMeta(context.Background(), &rpc.ContextMeta{Principal: p})
		return connectMetricQueryKey(ctx, &metricsv1.QueryMetricsRequest{})
	}
	if key("token-a") == key("token-b") {
		t.Fatal("different resource scopes share a cached response")
	}
}

func TestMetricQueryCacheByteBudgetAndReplacement(t *testing.T) {
	resetMetricQueryCache(t)
	now := time.Now()
	oversize := &metricsv1.QueryMetricsResponse{Series: []*metricsv1.MetricsSeries{{Metric: strings.Repeat("x", connectMetricQueryCacheEntryBytes+1)}}}
	storeMetricQueryMessage("large", oversize, now)
	if _, ok := cachedMetricQueryMessage("large", now); ok {
		t.Fatal("oversized response cached")
	}
	message := &metricsv1.QueryMetricsResponse{Series: []*metricsv1.MetricsSeries{{Metric: strings.Repeat("x", 1<<20)}}}
	for i := 0; i < 40; i++ {
		storeMetricQueryMessage(fmt.Sprint(i), message, now)
		connectMetricQueryCache.Lock()
		size := connectMetricQueryCache.bytes
		connectMetricQueryCache.Unlock()
		if size > connectMetricQueryCacheBytes {
			t.Fatalf("cache budget exceeded: %d", size)
		}
	}
	storeMetricQueryMessage("replace", message, now)
	storeMetricQueryMessage("replace", &metricsv1.QueryMetricsResponse{}, now)
	connectMetricQueryCache.Lock()
	var actual int
	for _, entry := range connectMetricQueryCache.items {
		actual += entry.size
	}
	if actual != connectMetricQueryCache.bytes {
		t.Errorf("accounted=%d actual=%d", connectMetricQueryCache.bytes, actual)
	}
	connectMetricQueryCache.Unlock()
	_, _ = cachedMetricQueryMessage("replace", now.Add(connectMetricCacheTTL))
}

func TestMetricQueryFollowerCanCancelWithoutWaitingForSharedRead(t *testing.T) {
	resetMetricQueryCache(t)
	parent, cancel := context.WithCancel(context.Background())
	ctx := &metricCancellationContext{Context: parent, reached: make(chan struct{})}
	defer cancel()
	req := connect.NewRequest(&metricsv1.QueryMetricsRequest{})
	key := connectMetricQueryKey(ctx, req.Msg)
	started, release := make(chan struct{}), make(chan struct{})
	leader := connectMetricQueryFlight.DoChan(key, func() (any, error) { close(started); <-release; return &metricsv1.QueryMetricsResponse{}, nil })
	defer func() { close(release); <-leader }()
	<-started
	done := make(chan error, 1)
	go func() { _, err := (&metricsService{}).QueryMetrics(ctx, req); done <- err }()
	select {
	case <-ctx.reached:
	case <-time.After(time.Second):
		t.Fatal("query did not join shared read")
	}
	cancel()
	select {
	case err := <-done:
		if connect.CodeOf(err) != connect.CodeCanceled {
			t.Fatalf("cancel result: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled follower waited for shared read")
	}
}

type metricCancellationContext struct {
	context.Context
	reached chan struct{}
	once    sync.Once
}

func (ctx *metricCancellationContext) Done() <-chan struct{} {
	ctx.once.Do(func() { close(ctx.reached) })
	return ctx.Context.Done()
}

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
