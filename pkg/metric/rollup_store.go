package metric

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"time"
)

// Bound each SQLite write transaction to a small time slice so live report
// writes can acquire the writer between backlog chunks. The rollup merge path
// consumes every raw sample exactly once, including when a one-hour bucket is
// assembled from several committed chunks.
const (
	metricCompactionChunkWindow  = 5 * time.Minute
	metricCompactionSeriesBatch  = 8
	metricCompactionChunksPerRun = 16
	metricCompactionVacuumEvery  = 16
	metricCompactionMinYield     = 100 * time.Millisecond
	// Scheduler compaction is time-budgeted rather than tied to a fixed number
	// of chunks. A throttled CPU therefore yields promptly while a fast host can
	// drain more backlog in the same invocation.
	//
	// The scheduler grants this budget per metric, so the deployment-wide cost of
	// one pass is this value times the number of metric definitions. A default
	// install defines about twenty metrics, so keep the slice small enough that a
	// full pass stays well inside maintenanceRunBudget instead of overrunning the
	// cron interval and making every pass collide with the next one.
	metricCompactionStepBudget = 400 * time.Millisecond
	// Sealing gets its own slice rather than sharing the chunk budget, which is
	// already spent by the time it runs. It walks series in batches with a yield
	// between them, so this has to cover several batches for the sweep to keep up
	// with ingest across a metric with many series.
	metricCompactionSealBudget = 600 * time.Millisecond
)

func rollupIntervalsOutsidePolicy(configured, effective []RollupTier) []time.Duration {
	active := make(map[time.Duration]struct{}, len(effective))
	for _, tier := range effective {
		active[tier.Interval] = struct{}{}
	}
	obsolete := make([]time.Duration, 0, len(configured))
	for _, tier := range configured {
		if _, ok := active[tier.Interval]; !ok {
			obsolete = append(obsolete, tier.Interval)
		}
	}
	return obsolete
}

func alignRollupRetentionCutoff(cutoff time.Time, nextInterval time.Duration) time.Time {
	if nextInterval <= 0 {
		return cutoff.UTC()
	}
	return time.Unix(0, floorDivNano(cutoff.UTC().UnixNano(), nextInterval.Nanoseconds())).UTC()
}

// rollupKey identifies one rollup cell. The tag dimension (tagsHash) is part of
// the key so points carrying different tags never collapse into the same bucket.
//
// rollupKey 标识一个 rollup 单元；tagsHash 是 key 的一部分，确保不同标签
// 的点不会落入同一个桶。
type rollupKey struct {
	// entityID is the entity dimension of the rollup cell.
	//
	// entityID 是 rollup 单元的实体维度。
	entityID string
	// tagsHash is the stable fingerprint of the tag set.
	//
	// tagsHash 是标签集合的稳定指纹。
	tagsHash string
	// bucket is the bucket start timestamp in nanoseconds.
	//
	// bucket 是桶起始时间的纳秒时间戳。
	bucket int64
}

// storedRollup represents a rollup row reconstructed from storage.
//
// storedRollup 表示从存储中还原的一行 rollup 数据。
type storedRollup struct {
	// entityID is the entity stored on the rollup row.
	//
	// entityID 是 rollup 行中保存的实体。
	entityID string
	// bucket is the stored bucket start timestamp in nanoseconds.
	//
	// bucket 是存储的桶起始纳秒时间戳。
	bucket int64
	// bucketData is the reconstructed in-memory accumulator for the row.
	//
	// bucketData 是该行还原出的内存累加器。
	bucketData *rollupBucket
}

// mergeRollupBucketsTx merges a consumed raw-data delta into the affected
// rollup buckets. Existing buckets are read by their indexed primary key, so a
// compact run performs work proportional to new/late data rather than to the
// complete retained history.
func (s *Store) mergeRollupBucketsTx(ctx context.Context, metricName string, interval time.Duration, buckets map[rollupKey]*rollupBucket, tx *sql.Tx) (int, error) {
	if len(buckets) == 0 {
		return 0, nil
	}

	keys := make([]rollupKey, 0, len(buckets))
	for key := range buckets {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].entityID != keys[j].entityID {
			return keys[i].entityID < keys[j].entityID
		}
		if keys[i].tagsHash != keys[j].tagsHash {
			return keys[i].tagsHash < keys[j].tagsHash
		}
		return keys[i].bucket < keys[j].bucket
	})

	stmt := ""
	if !s.sqliteStorageV4 {
		stmt = s.rollupUpsertSQL()
	}
	resolution := interval.Nanoseconds()
	createdAt := time.Now().UTC().UnixNano()
	for _, key := range keys {
		bucket := buckets[key]
		existing, err := s.readRollupBucketTx(ctx, metricName, key.entityID, key.tagsHash, interval, key.bucket, tx)
		if err != nil {
			return 0, err
		}
		if existing != nil {
			existing.mergeStored(bucket)
			bucket = existing
		}
		tagsJSON := bucket.tagsJSON
		if tagsJSON == "" {
			tagsJSON = "{}"
		}
		if s.sqliteStorageV4 {
			err = s.upsertSQLiteV8RollupValueTx(ctx, tx, metricName, key, tagsJSON, resolution, bucket, createdAt)
		} else {
			_, err = tx.ExecContext(ctx, stmt,
				metricName, key.entityID, key.tagsHash, tagsJSON, resolution, key.bucket,
				bucket.count, bucket.sum, bucket.sumSq, bucket.min, bucket.max,
				bucket.firstVal, bucket.firstTS, bucket.lastVal, bucket.lastTS,
				bucket.digest.Encode(), createdAt,
			)
		}
		if err != nil {
			return 0, err
		}
	}
	return len(keys), nil
}

// writeRollupBucketsTx upserts buckets within an existing transaction.
func (s *Store) writeRollupBucketsTx(ctx context.Context, metricName string, interval time.Duration, buckets map[rollupKey]*rollupBucket, tx *sql.Tx) (int, error) {
	return s.writeRollupBucketsWithMergePointTx(ctx, metricName, interval, buckets, time.Time{}, time.Time{}, nil, tx)
}

// writeRollupBucketsWithMergePointTx is the transactional version that executes
// all operations within an existing transaction. deltaCutoff marks coarser
// buckets whose source tier may already have aged out; a smaller recomputed
// bucket in that region is treated as late-arriving delta data and merged into
// the retained coarse bucket instead of replacing it.
func (s *Store) writeRollupBucketsWithMergePointTx(ctx context.Context, metricName string, interval time.Duration, buckets map[rollupKey]*rollupBucket, mergeCutoff, protectCutoff time.Time, deltaBuckets map[rollupKey]*rollupBucket, tx *sql.Tx) (int, error) {
	if len(buckets) == 0 {
		return 0, nil
	}
	keys := make([]rollupKey, 0, len(buckets))
	for k := range buckets {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].entityID != keys[j].entityID {
			return keys[i].entityID < keys[j].entityID
		}
		if keys[i].tagsHash != keys[j].tagsHash {
			return keys[i].tagsHash < keys[j].tagsHash
		}
		return keys[i].bucket < keys[j].bucket
	})

	stmt := ""
	if !s.sqliteStorageV4 {
		stmt = s.rollupUpsertSQL()
	}
	resNano := interval.Nanoseconds()
	now := time.Now().UTC().UnixNano()
	mergeCutoffNano := mergeCutoff.UnixNano()
	deltaCutoffNano := protectCutoff.UnixNano()
	written := 0

	for _, k := range keys {
		b := buckets[k]
		tagsJSON := b.tagsJSON
		if tagsJSON == "" {
			tagsJSON = "{}"
		}

		// If mergeCutoff is set and this bucket is older than the cutoff,
		// merge with existing data instead of replacing it.
		if !mergeCutoff.IsZero() && k.bucket < mergeCutoffNano {
			// Read existing row if it exists
			existing, err := s.readRollupBucketTx(ctx, metricName, k.entityID, k.tagsHash, interval, k.bucket, tx)
			if err != nil {
				return 0, err
			}
			if existing != nil {
				// Merge the new bucket into the existing one
				existing.mergeStored(b)
				b = existing
			}
		}
		if !protectCutoff.IsZero() && k.bucket < deltaCutoffNano {
			existing, err := s.readRollupBucketTx(ctx, metricName, k.entityID, k.tagsHash, interval, k.bucket, tx)
			if err != nil {
				return 0, err
			}
			if existing != nil {
				if deltaBuckets == nil {
					// Without delta tracking (raw is retained forever), the
					// source tier is rebuilt from full raw data and can replace.
				} else if delta := deltaBuckets[k]; delta != nil && delta.count > 0 {
					existing.mergeStored(delta)
					b = existing
				} else {
					continue
				}
			}
		}

		// Column order must match rollupColumns in dialect_rollup.go
		var err error
		if s.sqliteStorageV4 {
			err = s.upsertSQLiteV8RollupValueTx(ctx, tx, metricName, k, tagsJSON, resNano, b, now)
		} else {
			_, err = tx.ExecContext(ctx, stmt,
				metricName, k.entityID, k.tagsHash, tagsJSON, resNano, k.bucket,
				b.count, b.sum, b.sumSq, b.min, b.max,
				b.firstVal, b.firstTS, b.lastVal, b.lastTS,
				b.digest.Encode(), now,
			)
		}
		if err != nil {
			return 0, err
		}
		written++
	}
	return written, nil
}

// deleteRollupsBeforeTx deletes stored rollup rows within a transaction.
func (s *Store) deleteRollupsBeforeTx(ctx context.Context, metricName string, interval time.Duration, before time.Time, tx *sql.Tx) error {
	if s.sqliteStorageV4 {
		beforeNano := before.UTC().UnixNano()
		_, err := s.deleteSQLiteV4RollupsTx(ctx, tx, Query{MetricName: metricName}, []int64{interval.Nanoseconds()}, &beforeNano)
		return err
	}
	sqlText := fmt.Sprintf(
		`DELETE FROM %s WHERE metric_name = %s AND resolution_nano = %s AND bucket_nano < %s`,
		s.tables.rollups, s.dialect.placeholder(1), s.dialect.placeholder(2), s.dialect.placeholder(3),
	)
	_, err := tx.ExecContext(ctx, sqlText, metricName, interval.Nanoseconds(), before.UTC().UnixNano())
	return err
}

// floorDivNano floors ts to the start of its size-wide bucket, handling
// negative timestamps (pre-epoch) toward negative infinity so buckets align
// consistently. Mirrors alignTime but operates on raw nanos.
//
// floorDivNano 将 ts 向下对齐到 size 宽桶的起点，并把负时间戳（Unix epoch 前）
// 朝负无穷取整，让桶保持一致对齐。它与 alignTime 逻辑一致，但直接操作纳秒值。
func floorDivNano(ts, size int64) int64 {
	rem := ((ts % size) + size) % size
	return ts - rem
}

// readRollupBucketTx reads a single rollup bucket from a transaction.
func (s *Store) readRollupBucketTx(ctx context.Context, metricName, entityID, tagsHash string, interval time.Duration, bucketNano int64, tx *sql.Tx) (*rollupBucket, error) {
	if s.sqliteStorageV4 {
		return s.readSQLiteV4RollupBucketTx(ctx, tx, metricName, entityID, tagsHash, interval.Nanoseconds(), bucketNano)
	}
	sqlText := fmt.Sprintf(
		`SELECT count, sum, sum_sq, min_val, max_val, first_val, first_ts, last_val, last_ts, digest, tags
		 FROM %s WHERE metric_name = %s AND resolution_nano = %s AND entity_id = %s
		 AND tags_hash = %s AND bucket_nano = %s`,
		s.tables.rollups,
		s.dialect.placeholder(1), s.dialect.placeholder(2), s.dialect.placeholder(3),
		s.dialect.placeholder(4), s.dialect.placeholder(5),
	)
	row := tx.QueryRowContext(ctx, sqlText, metricName, interval.Nanoseconds(), entityID, tagsHash, bucketNano)

	var count int64
	var sum, sumSq, minV, maxV, firstV, lastV float64
	var firstTS, lastTS int64
	var digestBlob []byte
	var rawTags any

	err := row.Scan(&count, &sum, &sumSq, &minV, &maxV, &firstV, &firstTS, &lastV, &lastTS, &digestBlob, &rawTags)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	td, err := DecodeTDigest(digestBlob)
	if err != nil {
		return nil, err
	}
	tagsJSON, err := rawTagsToJSON(rawTags)
	if err != nil {
		return nil, err
	}

	return &rollupBucket{
		count: count, sum: sum, sumSq: sumSq,
		min: minV, max: maxV,
		firstVal: firstV, firstTS: firstTS,
		lastVal: lastV, lastTS: lastTS,
		digest:   td,
		tagsHash: tagsHash,
		tagsJSON: tagsJSON,
	}, nil
}
