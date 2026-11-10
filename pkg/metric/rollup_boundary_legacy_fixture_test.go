//go:build metric_legacy_fixture

package metric

import (
	"context"
	"math"
	"testing"
	"time"
)

// TestAggregateRollupEndBoundaryInclusive verifies the rollup query end boundary
// is inclusive (matching the raw query's ts <= end) and that a window aligned to
// resolution boundaries returns every whole bucket it covers.
//
// Buckets at 00:00 and 00:01 are both whole-contained in [00:00:00, 00:02:00]:
// the second bucket's window is [00:01:00, 00:02:00), whose last covered nano is
// < 00:02:00, so an inclusive end keeps it. The previous half-open (< end) rule
// could drop a bucket sitting exactly on the end edge.
//
// TestAggregateRollupEndBoundaryInclusive 验证 rollup 查询 end 边界为闭区间
// （与 raw 的 ts <= end 一致），且对齐到分辨率边界的窗口会返回它覆盖的每个完整桶。
func TestAggregateRollupEndBoundaryInclusive(t *testing.T) {
	ctx := context.Background()
	policy := RollupPolicy{Tiers: []RollupTier{{Interval: time.Minute, Retention: 24 * time.Hour}}}
	s := newRollupStore(t, policy)
	if err := s.CreateMetric(ctx, Definition{Name: "edge", Type: TypeGauge, RetentionDays: 30}); err != nil {
		t.Fatalf("create: %v", err)
	}
	base := time.Date(2026, 6, 18, 0, 0, 0, 0, time.UTC)
	batch := []Point{
		{MetricName: "edge", EntityID: "n1", Timestamp: base.Add(10 * time.Second), Value: 1},             // bucket 00:00
		{MetricName: "edge", EntityID: "n1", Timestamp: base.Add(time.Minute + 10*time.Second), Value: 2}, // bucket 00:01
	}
	if err := s.WriteBatch(ctx, batch); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := s.Compact(ctx, base.Add(time.Hour)); err != nil {
		t.Fatalf("compact: %v", err)
	}

	res, err := s.AggregateRollup(ctx, AggregateQuery{
		Query:       Query{MetricName: "edge", EntityID: "n1", Start: base, End: base.Add(2 * time.Minute)},
		Aggregation: AggSum,
		Interval:    time.Minute,
	}, time.Minute)
	if err != nil {
		t.Fatalf("aggregate rollup: %v", err)
	}
	if len(res) != 2 {
		t.Fatalf("aligned window should return both whole buckets, got %d: %#v", len(res), res)
	}
	if res[0].Value != 1 || res[1].Value != 2 {
		t.Fatalf("unexpected bucket values: %#v", res)
	}
}

// TestSeriesHybridMergesStraddlingBucket verifies a query output bucket that
// straddles the raw-retention boundary aggregates BOTH its rollup half and its
// raw half, instead of letting raw replace the rollup half.
//
// With a 30m raw retention and now=12:00 the boundary is 11:30. A 1h output
// bucket [11:00, 12:00) therefore straddles it. An 11:05 sample (value 10, older
// than the boundary -> rolled up, raw deleted) and an 11:35 sample (value 100,
// within retention -> still raw) both belong to that 11:00 bucket. The correct
// answer folds both: count=2, avg=(10+100)/2=55. The old merge-by-bucket-time
// path kept only the raw half and reported count=1, avg=100.
//
// TestSeriesHybridMergesStraddlingBucket 验证跨越原始保留期边界的输出桶会同时
// 聚合 rollup 半边和 raw 半边，而不是让 raw 覆盖 rollup 半边。
func TestSeriesHybridMergesStraddlingBucket(t *testing.T) {
	ctx := context.Background()
	policy := RollupPolicy{
		RawRetention: 30 * time.Minute,
		Tiers:        []RollupTier{{Interval: time.Minute, Retention: 24 * time.Hour}},
	}
	s := newRollupStore(t, policy)
	if err := s.CreateMetric(ctx, Definition{Name: "straddle", Type: TypeGauge, RetentionDays: 30}); err != nil {
		t.Fatalf("create: %v", err)
	}
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	// 11:05 is older than the 11:30 cutoff (rolled up, raw deleted on compact);
	// 11:35 is within retention (kept raw).
	old := now.Add(-55 * time.Minute)    // 11:05
	recent := now.Add(-25 * time.Minute) // 11:35
	batch := []Point{
		{MetricName: "straddle", EntityID: "n1", Timestamp: old, Value: 10},
		{MetricName: "straddle", EntityID: "n1", Timestamp: recent, Value: 100},
	}
	if err := s.WriteBatch(ctx, batch); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := s.Compact(ctx, now); err != nil {
		t.Fatalf("compact: %v", err)
	}

	// Confirm the old raw point was deleted and the recent one survives, so the
	// two halves genuinely come from different sources.
	oldRaw, err := s.Query(ctx, Query{MetricName: "straddle", EntityID: "n1", Start: old.Add(-time.Minute), End: old.Add(time.Minute)})
	if err != nil {
		t.Fatalf("query old raw: %v", err)
	}
	if len(oldRaw) != 0 {
		t.Fatalf("expected old raw point deleted by retention, got %d", len(oldRaw))
	}

	got, err := s.Series(ctx, AggregateQuery{
		Query:       Query{MetricName: "straddle", EntityID: "n1", Start: now.Add(-time.Hour), End: now},
		Aggregation: AggAvg,
		Interval:    time.Hour,
	}, now)
	if err != nil {
		t.Fatalf("series: %v", err)
	}

	bucket := alignTime(now.Add(-time.Hour), time.Hour) // 11:00
	var found bool
	for _, p := range got {
		if p.Bucket.Equal(bucket) {
			found = true
			if p.Count != 2 {
				t.Fatalf("straddling bucket count: got %d want 2 (%#v)", p.Count, p)
			}
			if math.Abs(p.Value-55) > 1e-9 {
				t.Fatalf("straddling bucket avg: got %v want 55 (%#v)", p.Value, p)
			}
		}
	}
	if !found {
		t.Fatalf("expected a merged 11:00 bucket in result: %#v", got)
	}
}
