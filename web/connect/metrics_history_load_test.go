package connectapi

import (
	"context"
	"errors"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/komari-monitor/komari/pkg/metric"
)

// An opt-in measurement, not a machine-speed assertion. Run previous/grouped
// in separate processes to keep RSS high-water observations distinguishable.
func TestHistoryConcurrentReadAndIngestionExperiment(t *testing.T) {
	if os.Getenv("KOMARI_HISTORY_PERF") != "1" {
		t.Skip("set KOMARI_HISTORY_PERF=1 for the file-backed load experiment")
	}
	mode := os.Getenv("KOMARI_HISTORY_PARTITION")
	if mode != "previous" && mode != "grouped" {
		t.Fatal("set KOMARI_HISTORY_PARTITION=previous or grouped")
	}
	store, names, ids, end := historyReadFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	jobs := make(chan time.Duration, 12)
	for range 4 {
		for _, window := range []time.Duration{time.Hour, 24 * time.Hour, 30 * 24 * time.Hour} {
			jobs <- window
		}
	}
	close(jobs)
	var workers sync.WaitGroup
	var mu sync.Mutex
	var readLatency, ingestLatency []time.Duration
	failures := make(chan error, 4)
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	var peak atomic.Uint64
	peak.Store(before.HeapAlloc)
	var background sync.WaitGroup
	background.Add(2)
	go func() {
		defer background.Done()
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				var m runtime.MemStats
				runtime.ReadMemStats(&m)
				for old := peak.Load(); m.HeapAlloc > old; old = peak.Load() {
					if peak.CompareAndSwap(old, m.HeapAlloc) {
						break
					}
				}
			}
		}
	}()
	go func() {
		defer background.Done()
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		i := 0
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				i++
				started := time.Now()
				err := store.Write(ctx, metric.Point{MetricName: names[0], EntityID: ids[0], Timestamp: end.Add(time.Duration(i) * time.Second), Value: float64(i)})
				if err != nil {
					if ctx.Err() == nil {
						failures <- err
					}
					return
				}
				ingestLatency = append(ingestLatency, time.Since(started))
			}
		}
	}()
	for range 2 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for window := range jobs {
				queries := make([]metric.AggregateQuery, 0, len(names))
				for _, name := range names {
					queries = append(queries, metric.AggregateQuery{Query: metric.Query{MetricName: name, Start: end.Add(-window), End: end, Order: metric.OrderAsc}, Aggregation: metric.AggAvg, Interval: metric.CeilStandardInterval(window / 500), PreserveSeries: true})
				}
				started := time.Now()
				batch, err := store.DashboardSeriesBatch(ctx, queries, end)
				if err != nil {
					failures <- err
					return
				}
				n := 0
				for _, points := range batch {
					if mode == "grouped" {
						index := connectAggregatePointIndices(points, ids...)
						for _, id := range ids {
							n += len(index[id])
						}
					} else {
						for _, id := range ids {
							n += len(legacyEntityPartition(points, id))
						}
					}
				}
				if n == 0 {
					failures <- errors.New("fixture produced no results")
					return
				}
				mu.Lock()
				readLatency = append(readLatency, time.Since(started))
				mu.Unlock()
			}
		}()
	}
	workers.Wait()
	cancel()
	background.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	if len(readLatency) != 12 || len(ingestLatency) == 0 {
		t.Fatalf("reads=%d writes=%d", len(readLatency), len(ingestLatency))
	}
	p := func(values []time.Duration, fraction float64) time.Duration {
		sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
		return values[int(float64(len(values)-1)*fraction)]
	}
	t.Logf("mode=%s readers=2 queries=%d writes=%d query-p50=%s query-p95=%s ingest-p50=%s ingest-p95=%s peak-go-heap=%d baseline-go-heap=%d", mode, len(readLatency), len(ingestLatency), p(readLatency, .5), p(readLatency, .95), p(ingestLatency, .5), p(ingestLatency, .95), peak.Load(), before.HeapAlloc)
	if status, err := os.ReadFile("/proc/self/status"); err == nil {
		for _, line := range strings.Split(string(status), "\n") {
			if strings.HasPrefix(line, "VmHWM:") {
				t.Log("process RSS high water (includes fixture setup):", line)
			}
		}
	}
	cancelled, stop := context.WithCancel(context.Background())
	stop()
	q := []metric.AggregateQuery{{Query: metric.Query{MetricName: names[0], Start: end.Add(-time.Hour), End: end}, Aggregation: metric.AggAvg, Interval: time.Minute}}
	if _, err := store.DashboardSeriesBatch(cancelled, q, end); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled scan: %v", err)
	}
	if _, err := store.DashboardSeriesBatch(context.Background(), q, end); err != nil {
		t.Fatalf("read slot leaked after cancellation: %v", err)
	}
}
