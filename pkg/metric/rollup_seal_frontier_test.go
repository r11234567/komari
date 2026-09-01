package metric

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestRollupSealCutoffClampsToClosedBuckets(t *testing.T) {
	const resolution = int64(time.Hour)
	frontier := time.Date(2026, 8, 1, 5, 30, 0, 0, time.UTC).UnixNano()
	before := time.Date(2026, 8, 1, 7, 0, 0, 0, time.UTC).UnixNano()

	cutoff, ok := rollupSealCutoff(before, frontier, resolution)
	if !ok {
		t.Fatal("a frontier inside the retained window must still allow sealing")
	}
	// Raw data is consumed through 05:30. The 04:00 bucket closed at 05:00 and is
	// final; the 05:00 bucket does not close until 06:00 and is still collecting.
	closedBucket := time.Date(2026, 8, 1, 4, 0, 0, 0, time.UTC).UnixNano()
	openBucket := time.Date(2026, 8, 1, 5, 0, 0, 0, time.UTC).UnixNano()
	if closedBucket >= cutoff {
		t.Fatalf("closed bucket %d must fall below cutoff %d", closedBucket, cutoff)
	}
	if openBucket < cutoff {
		t.Fatalf("open bucket %d must not fall below cutoff %d", openBucket, cutoff)
	}

	if got, ok := rollupSealCutoff(before, sqliteV4NoRollupFrontier, resolution); !ok || got != before {
		t.Fatalf("unclamped cutoff = %d (ok=%v), want %d", got, ok, before)
	}
	// A frontier past the caller's own bound must never widen it.
	if got, ok := rollupSealCutoff(before, before+2*resolution, resolution); !ok || got != before {
		t.Fatalf("cutoff = %d (ok=%v), want the caller bound %d", got, ok, before)
	}
}

// TestPreserveRawCompactionLeavesOpenCoarseBucketsHot pins the behaviour that
// keeps steady-state maintenance cheap. Compaction materializes buckets at the
// raw frontier, which the materialization delay places hours behind wall clock,
// so a coarse bucket looks long past the seal window while it is still being
// filled minute by minute. Sealing it early makes every later update decode and
// re-encode the compressed block that now holds it, which is pure churn: the
// bucket count does not grow, only the rewrite cost.
func TestPreserveRawCompactionLeavesOpenCoarseBucketsHot(t *testing.T) {
	ctx := context.Background()
	policy := RollupPolicy{
		Mode:         RollupModePreserveRaw,
		PreserveRaw:  true,
		RawRetention: 2 * time.Hour,
		Tiers: []RollupTier{
			{Interval: time.Minute, Retention: 6 * time.Hour},
			{Interval: time.Hour, Retention: 30 * 24 * time.Hour},
		},
	}
	store := newRollupStore(t, policy)
	if !store.sqliteStorageV4 {
		t.Fatal("regression must exercise SQLite V4 sealed rollup blocks")
	}

	const metricName = "open-coarse-bucket"
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	if err := store.CreateMetric(ctx, Definition{Name: metricName, Type: TypeGauge, RetentionDays: 30}); err != nil {
		t.Fatalf("create metric: %v", err)
	}
	points := make([]Point, 0, 6*60)
	for i := 0; i < 6*60; i++ {
		points = append(points, Point{
			MetricName: metricName,
			EntityID:   "node-a",
			Timestamp:  base.Add(time.Duration(i) * time.Minute),
			Value:      float64(i + 1),
		})
	}
	if err := store.WriteBatch(ctx, points); err != nil {
		t.Fatalf("seed points: %v", err)
	}

	// Walk the frontier to 02:05, which closes the 00:00 and 01:00 hour buckets
	// and opens the 02:00 one. Drain in a loop because one step is time-budgeted.
	initialNow := base.Add(4*time.Hour + 5*time.Minute)
	initialCutoff := policy.withMetricRetention(30 * 24 * time.Hour).rawCutoff(initialNow)
	drained := false
	for i := 0; i < 64 && !drained; i++ {
		if _, err := store.CompactMetricStep(ctx, metricName, initialNow); err != nil {
			t.Fatalf("initial compaction pass %d: %v", i, err)
		}
		watermark, ok, err := store.compactionWatermark(ctx, metricName)
		if err != nil {
			t.Fatalf("read watermark: %v", err)
		}
		drained = ok && !watermark.Before(initialCutoff)
	}
	if !drained {
		t.Fatal("initial compaction never reached the raw cutoff")
	}
	const hourResolution = int64(time.Hour)
	openBucketStart := base.Add(2 * time.Hour)
	openBucket := openBucketStart.UnixNano()
	closedBucket := base.Add(time.Hour).UnixNano()

	if blocks := countRollupBlocksCovering(t, store, metricName, hourResolution, closedBucket); blocks == 0 {
		t.Fatal("a closed hour bucket should be sealed into a compressed block")
	}
	blocksBefore := snapshotRollupBlocks(t, store, metricName, hourResolution)

	// Advance the way the cron does: one minute per pass, all inside the same
	// hour bucket, so no new hour closes and no new hour bucket is produced.
	for i := 1; i <= 20; i++ {
		at := base.Add(4*time.Hour + 5*time.Minute + time.Duration(i)*time.Minute)
		if _, err := store.CompactMetricStep(ctx, metricName, at); err != nil {
			t.Fatalf("compaction pass %d: %v", i, err)
		}
	}

	if got := countRollupValues(t, store, metricName, hourResolution, openBucket); got != 1 {
		t.Fatalf("open hour bucket rows in the hot table = %d, want 1: an open bucket must stay hot", got)
	}
	if got := countRollupBlocksCovering(t, store, metricName, hourResolution, openBucket); got != 0 {
		t.Fatalf("open hour bucket is covered by %d sealed block(s), want 0", got)
	}
	blocksAfter := snapshotRollupBlocks(t, store, metricName, hourResolution)
	for start, checksum := range blocksBefore {
		after, ok := blocksAfter[start]
		if !ok {
			t.Fatalf("sealed hour block start=%d disappeared across passes", start)
		}
		if after != checksum {
			t.Fatalf("sealed hour block start=%d was rewritten (checksum %d -> %d) without producing a new bucket", start, checksum, after)
		}
	}

	// One raw point per minute, so the bucket holds every minute between its
	// start and the frontier compaction has reached.
	watermark, ok, err := store.compactionWatermark(ctx, metricName)
	if err != nil || !ok {
		t.Fatalf("read final watermark: ok=%v err=%v", ok, err)
	}
	wantCount := int64(watermark.Sub(openBucketStart) / time.Minute)
	if wantCount <= 0 {
		t.Fatalf("frontier %s never entered the open bucket at %s", watermark, openBucketStart)
	}
	tagsHash := seriesTagsHash(t, store, metricName, "node-a")

	// The bucket must remain readable and complete while it is held hot.
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	bucket, err := store.readSQLiteV4RollupBucketTx(ctx, tx, metricName, "node-a", tagsHash, hourResolution, openBucket)
	if err != nil {
		t.Fatalf("read open hour bucket: %v", err)
	}
	if bucket == nil {
		t.Fatal("open hour bucket must be readable while it is held hot")
	}
	if bucket.count != wantCount {
		t.Fatalf("open hour bucket count = %d, want %d minutes materialized so far", bucket.count, wantCount)
	}
}

func seriesTagsHash(t *testing.T, store *Store, metricName, entityID string) string {
	t.Helper()
	var tagsHash string
	query := fmt.Sprintf(
		`SELECT tags_hash FROM %s WHERE metric_name = ? AND entity_id = ?`, store.tables.series,
	)
	if err := store.db.QueryRow(query, metricName, entityID).Scan(&tagsHash); err != nil {
		t.Fatalf("read series tags hash: %v", err)
	}
	return tagsHash
}

func countRollupValues(t *testing.T, store *Store, metricName string, resolution, bucketNano int64) int {
	t.Helper()
	var count int
	query := fmt.Sprintf(
		`SELECT COUNT(*) FROM %s AS v JOIN %s AS s ON s.id = v.series_id
		 WHERE s.metric_name = ? AND v.resolution_nano = ? AND v.bucket_nano = ?`,
		store.tables.rollupValues, store.tables.series,
	)
	if err := store.db.QueryRow(query, metricName, resolution, bucketNano).Scan(&count); err != nil {
		t.Fatalf("count hot rollup rows: %v", err)
	}
	return count
}

func countRollupBlocksCovering(t *testing.T, store *Store, metricName string, resolution, bucketNano int64) int {
	t.Helper()
	var count int
	query := fmt.Sprintf(
		`SELECT COUNT(*) FROM %s AS b JOIN %s AS s ON s.id = b.series_id
		 WHERE s.metric_name = ? AND b.resolution_nano = ? AND b.start_nano <= ? AND b.end_nano >= ?`,
		store.tables.rollupBlocks, store.tables.series,
	)
	if err := store.db.QueryRow(query, metricName, resolution, bucketNano, bucketNano).Scan(&count); err != nil {
		t.Fatalf("count covering rollup blocks: %v", err)
	}
	return count
}

func snapshotRollupBlocks(t *testing.T, store *Store, metricName string, resolution int64) map[int64]int64 {
	t.Helper()
	query := fmt.Sprintf(
		`SELECT b.start_nano, b.checksum FROM %s AS b JOIN %s AS s ON s.id = b.series_id
		 WHERE s.metric_name = ? AND b.resolution_nano = ?`,
		store.tables.rollupBlocks, store.tables.series,
	)
	rows, err := store.db.Query(query, metricName, resolution)
	if err != nil {
		t.Fatalf("snapshot rollup blocks: %v", err)
	}
	defer rows.Close()
	snapshot := make(map[int64]int64)
	for rows.Next() {
		var start, checksum int64
		if err := rows.Scan(&start, &checksum); err != nil {
			t.Fatalf("scan rollup block: %v", err)
		}
		snapshot[start] = checksum
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate rollup blocks: %v", err)
	}
	return snapshot
}
