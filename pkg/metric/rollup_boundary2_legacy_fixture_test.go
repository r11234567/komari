//go:build metric_legacy_fixture

package metric

import (
	"context"
	"testing"
	"time"
)

// TestSeriesHybridUsesLastCompactionWatermark verifies that a query does not
// advance the raw/rollup split beyond the last successful compaction. The raw
// point at 11:32 is still present after the 12:00 compaction, but a query at
// 12:04 would calculate a dynamic cutoff of 11:34 and skip it without the
// persisted watermark at 11:30.
func TestSeriesHybridUsesLastCompactionWatermark(t *testing.T) {
	ctx := context.Background()
	policy := RollupPolicy{
		RawRetention: 30 * time.Minute,
		Tiers:        []RollupTier{{Interval: time.Minute, Retention: 24 * time.Hour}},
	}
	s := newRollupStore(t, policy)
	if err := s.CreateMetric(ctx, Definition{Name: "watermark", Type: TypeGauge, RetentionDays: 30}); err != nil {
		t.Fatalf("create: %v", err)
	}

	compactAt := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	if err := s.WriteBatch(ctx, []Point{
		{MetricName: "watermark", EntityID: "n1", Timestamp: time.Date(2026, 6, 18, 11, 20, 0, 0, time.UTC), Value: 10},
		{MetricName: "watermark", EntityID: "n1", Timestamp: time.Date(2026, 6, 18, 11, 32, 0, 0, time.UTC), Value: 100},
	}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := s.Compact(ctx, compactAt); err != nil {
		t.Fatalf("compact: %v", err)
	}

	queryAt := compactAt.Add(4 * time.Minute)
	got, err := s.Series(ctx, AggregateQuery{
		Query:       Query{MetricName: "watermark", EntityID: "n1", Start: time.Date(2026, 6, 18, 11, 0, 0, 0, time.UTC), End: queryAt},
		Aggregation: AggCount,
		Interval:    time.Hour,
	}, queryAt)
	if err != nil {
		t.Fatalf("series: %v", err)
	}
	if len(got) != 1 || got[0].Count != 2 {
		t.Fatalf("series should include rollup and raw data before the next compaction, got %#v", got)
	}

	watermark, found, err := s.compactionWatermark(ctx, "watermark")
	if err != nil {
		t.Fatalf("read watermark: %v", err)
	}
	if !found || !watermark.Equal(policy.rawCutoff(compactAt)) {
		t.Fatalf("watermark = %v, found=%v, want %v", watermark, found, policy.rawCutoff(compactAt))
	}
}

func TestSeriesWithoutWatermarkDoesNotIncludePartialRollupBucket(t *testing.T) {
	ctx := context.Background()
	policy := RollupPolicy{
		RawRetention: 30 * time.Minute,
		Tiers:        []RollupTier{{Interval: time.Minute, Retention: 24 * time.Hour}},
	}
	s := newRollupStore(t, policy)
	if err := s.CreateMetric(ctx, Definition{Name: "upgrade-partial", Type: TypeGauge, RetentionDays: 30}); err != nil {
		t.Fatalf("create: %v", err)
	}

	compactAt := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	if err := s.WriteBatch(ctx, []Point{
		{MetricName: "upgrade-partial", EntityID: "n1", Timestamp: time.Date(2026, 6, 18, 11, 20, 10, 0, time.UTC), Value: 10},
		{MetricName: "upgrade-partial", EntityID: "n1", Timestamp: time.Date(2026, 6, 18, 11, 20, 50, 0, time.UTC), Value: 50},
	}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := s.Compact(ctx, compactAt); err != nil {
		t.Fatalf("compact: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM `+s.tables.watermarks+` WHERE metric_name = ?`, "upgrade-partial"); err != nil {
		t.Fatalf("remove watermark: %v", err)
	}

	got, err := s.Series(ctx, AggregateQuery{
		Query: Query{
			MetricName: "upgrade-partial",
			EntityID:   "n1",
			Start:      time.Date(2026, 6, 18, 11, 20, 0, 0, time.UTC),
			End:        time.Date(2026, 6, 18, 11, 20, 20, 0, time.UTC),
		},
		Aggregation: AggCount,
		Interval:    time.Minute,
	}, compactAt.Add(4*time.Minute))
	if err != nil {
		t.Fatalf("series: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("partial rollup bucket leaked out-of-window points: %#v", got)
	}
}
