package connectapi

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/komari-monitor/komari/pkg/metric"
	metricsv1 "github.com/r11234567/komari-proto/gen/go/komari/metrics/v1"
	"google.golang.org/protobuf/proto"
)

// Keep the prior implementation here as a benchmark/reference, not a second
// production path. Both alternatives consume the identical ordered samples.
func legacyEntityPartition(points []metric.AggregatePoint, id string) []metric.AggregatePoint {
	result := make([]metric.AggregatePoint, 0, len(points))
	for _, p := range points {
		if p.EntityID == id {
			result = append(result, p)
		}
	}
	return result
}

var historyPartitionSink int

func BenchmarkHistoryEntityPartition(b *testing.B) {
	for _, nodes := range []int{1, 8, 32} {
		for _, window := range []struct {
			name  string
			count int
		}{{"1h", 12}, {"24h", 288}, {"30d", 500}} {
			b.Run(fmt.Sprintf("%dnodes/%s", nodes, window.name), func(b *testing.B) {
				ids := make([]string, nodes)
				points := make([]metric.AggregatePoint, 0, nodes*window.count)
				for node := range nodes {
					ids[node] = fmt.Sprintf("node-%d", node)
				}
				for sample := range window.count {
					for _, id := range ids {
						points = append(points, metric.AggregatePoint{EntityID: id, Value: float64(sample), Count: 1})
					}
				}
				b.Run("previous", func(b *testing.B) {
					b.ReportAllocs()
					for range b.N {
						n := 0
						for _, id := range ids {
							n += len(legacyEntityPartition(points, id))
						}
						historyPartitionSink = n
					}
				})
				b.Run("grouped", func(b *testing.B) {
					b.ReportAllocs()
					for range b.N {
						n := 0
						grouped := connectAggregatePointIndices(points, ids...)
						for _, id := range ids {
							n += len(grouped[id])
						}
						historyPartitionSink = n
					}
				})
			})
		}
	}
}

// A deterministic file-backed V4 fixture. The two alternatives read the same
// warm SQLite data with the response cache bypassed; setup is outside timing.
func BenchmarkHistoryReadPartition(b *testing.B) {
	ctx := context.Background()
	s, names, ids, end := historyReadFixture(b)
	for _, window := range []struct {
		name     string
		duration time.Duration
	}{{"1h", time.Hour}, {"24h", 24 * time.Hour}, {"30d", 30 * 24 * time.Hour}} {
		b.Run(window.name, func(b *testing.B) {
			queries := make([]metric.AggregateQuery, 0, len(names))
			for _, name := range names {
				queries = append(queries, metric.AggregateQuery{Query: metric.Query{MetricName: name, Start: end.Add(-window.duration), End: end, Order: metric.OrderAsc}, Aggregation: metric.AggAvg, Interval: metric.CeilStandardInterval(window.duration / 500), PreserveSeries: true})
			}
			if _, err := s.DashboardSeriesBatch(ctx, queries, end); err != nil {
				b.Fatal(err)
			}
			for _, grouped := range []bool{false, true} {
				label := "previous"
				if grouped {
					label = "grouped"
				}
				b.Run(label, func(b *testing.B) {
					b.ReportAllocs()
					for range b.N {
						batch, err := s.DashboardSeriesBatch(ctx, queries, end)
						if err != nil {
							b.Fatal(err)
						}
						n := 0
						for _, points := range batch {
							if grouped {
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
						historyPartitionSink = n
					}
				})
			}
		})
	}
}

func TestMetricPartitionPreservesValuesTagsOrderAndIsolation(t *testing.T) {
	points := []metric.AggregatePoint{{EntityID: "a", Value: 1, Count: 2, Tags: map[string]string{"device": "x"}}, {EntityID: "b", Value: 9, Count: 3}, {EntityID: "a", Value: 4, Count: 5, Tags: map[string]string{"device": "y"}}}
	index := connectAggregatePointIndices(points)
	projected := connectAggregatePointIndices(points, "a", "absent")
	if _, ok := projected["b"]; ok || len(projected["a"]) != 2 || len(projected["absent"]) != 0 {
		t.Fatal("projection retained unrequested nodes")
	}
	raw := connectRawPointIndices([]metric.Point{{EntityID: "a", Value: 1}, {EntityID: "b", Value: 2}}, "a")
	if len(raw) != 1 || len(raw["a"]) != 1 {
		t.Fatal("raw projection retained unrequested nodes")
	}
	for _, id := range []string{"a", "b", "absent"} {
		want := legacyEntityPartition(points, id)
		if len(want) == 0 {
			if len(index[id]) != 0 {
				t.Fatal("unexpected empty-series points")
			}
			continue
		}
		got := make([]metric.AggregatePoint, 0, len(index[id]))
		for _, i := range index[id] {
			got = append(got, points[i])
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("partition differs for %s", id)
		}
	}
	index["a"][0] = 2
	if points[0].Value != 1 || index["b"][0] != 1 {
		t.Fatal("partition slices alias other series")
	}
	if len(connectAggregatePointIndices(nil)) != 0 || len(connectRawPointIndices(nil)) != 0 {
		t.Fatal("empty input not preserved")
	}
}

func TestIndexedMetricResponseMatchesPreviousProjection(t *testing.T) {
	base := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	points := []metric.AggregatePoint{{EntityID: "a", Bucket: base, Value: 1, Count: 2, Tags: map[string]string{"device": "x"}}, {EntityID: "b", Bucket: base, Value: 9, Count: 3}, {EntityID: "a", Bucket: base.Add(time.Minute), Value: 4, Count: 5, Tags: map[string]string{"device": "y"}}}
	indices := connectAggregatePointIndices(points, "a", "b", "absent")
	definition := metric.Definition{Type: metric.TypeGauge, Unit: "percent", RetentionDays: 90}
	compare := func(got, want []*metricsv1.MetricsSeries) {
		t.Helper()
		order := func(s []*metricsv1.MetricsSeries) {
			sort.Slice(s, func(i, j int) bool { return metricTagsKey(s[i].Labels) < metricTagsKey(s[j].Labels) })
		}
		order(got)
		order(want)
		if len(got) != len(want) {
			t.Fatal("series count changed")
		}
		for i := range got {
			if !proto.Equal(got[i], want[i]) {
				t.Fatal("wire response changed")
			}
		}
	}
	for _, id := range []string{"a", "b", "absent"} {
		for _, aggregation := range []metric.Aggregation{metric.AggAvg, metric.AggMin, metric.AggMax, metric.AggP50, metric.AggP99, metric.AggLast} {
			for _, fill := range []bool{false, true} {
				want := connectAggregateSeries("cpu", id, definition, aggregation, time.Minute, fill, base, base.Add(time.Hour), legacyEntityPartition(points, id))
				got := connectAggregateSeries("cpu", id, definition, aggregation, time.Minute, fill, base, base.Add(time.Hour), points, indices[id])
				compare(got, want)
			}
		}
	}
	raw := []metric.Point{{EntityID: "a", Timestamp: base, Value: 1, Tags: map[string]string{"d": "x"}, Labels: map[string]string{"l": "one"}}, {EntityID: "b", Timestamp: base, Value: 2}, {EntityID: "a", Timestamp: base.Add(time.Minute), Value: 3}}
	rawIndices := connectRawPointIndices(raw, "a", "b", "absent")
	for _, id := range []string{"a", "b", "absent"} {
		var previous []metric.Point
		for _, point := range raw {
			if point.EntityID == id {
				previous = append(previous, point)
			}
		}
		compare(connectRawSeries("cpu", id, definition, raw, rawIndices[id]), connectRawSeries("cpu", id, definition, previous))
	}
}

func historyReadFixture(tb testing.TB) (*metric.Store, []string, []string, time.Time) {
	tb.Helper()
	ctx := context.Background()
	s, err := metric.Open(ctx, metric.SQLite(filepath.Join(tb.TempDir(), "history.db"), metric.WithMaxOpenConns(4)))
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = s.Close() })
	names := []string{"cpu", "memory", "disk"}
	ids := []string{"a", "b", "c", "d"}
	for _, name := range names {
		if err = s.CreateMetric(ctx, metric.Definition{Name: name, Type: metric.TypeGauge, RetentionDays: 90}); err != nil {
			tb.Fatal(err)
		}
	}
	end := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	begin := end.Add(-30 * 24 * time.Hour)
	points := make([]metric.Point, 0, 4096)
	flush := func() {
		if err := s.WriteBatch(ctx, points); err != nil {
			tb.Fatal(err)
		}
		points = points[:0]
	}
	for ts := begin; !ts.After(end); ts = ts.Add(5 * time.Minute) {
		for _, name := range names {
			for _, id := range ids {
				points = append(points, metric.Point{MetricName: name, EntityID: id, Timestamp: ts, Value: float64(ts.Unix() % 97), Tags: map[string]string{"device": "main"}})
			}
		}
		if len(points) >= 4000 {
			flush()
		}
	}
	if len(points) > 0 {
		flush()
	}
	return s, names, ids, end
}
