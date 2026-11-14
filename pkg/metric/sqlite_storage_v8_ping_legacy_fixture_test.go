//go:build metric_legacy_fixture

package metric

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSQLiteV8MigratesLegacyDualPingPointsAtomically(t *testing.T) {
	ctx := context.Background()
	dsn := sqliteFileDSN(filepath.Join(t.TempDir(), "metrics.db"))
	legacy := createSQLiteV8LegacyStore(t, ctx, dsn)
	base := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	for _, definition := range []Definition{
		{Name: sqliteMergedPingLatencyMetric, Type: TypeGauge, RetentionDays: 19},
		{Name: sqliteVirtualPingLossMetric, Type: TypeGauge, RetentionDays: 19},
	} {
		if err := legacy.CreateMetric(ctx, definition); err != nil {
			t.Fatal(err)
		}
	}
	points := []Point{
		{MetricName: sqliteMergedPingLatencyMetric, EntityID: "node-a", Timestamp: base, Value: 21},
		{MetricName: sqliteVirtualPingLossMetric, EntityID: "node-a", Timestamp: base, Value: 0},
		{MetricName: sqliteMergedPingLatencyMetric, EntityID: "node-a", Timestamp: base.Add(5 * time.Second), Value: -1},
		{MetricName: sqliteVirtualPingLossMetric, EntityID: "node-a", Timestamp: base.Add(5 * time.Second), Value: 1},
	}
	if err := legacy.WriteBatch(ctx, points); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	var phases []string
	store, err := Open(ctx, SQLite(dsn, WithMigrationProgress(func(progress MigrationProgress) {
		phases = append(phases, progress.Phase)
	})))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if !store.sqlitePingMerged || !containsString(phases, MigrationPhaseMergingPingSeries) {
		t.Fatalf("V8 migration state merged=%v phases=%v", store.sqlitePingMerged, phases)
	}
	var userVersion, physicalLossSeries int
	if err := store.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&userVersion); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM metric_series WHERE metric_name = ?`, sqliteVirtualPingLossMetric).Scan(&physicalLossSeries); err != nil {
		t.Fatal(err)
	}
	if userVersion != sqliteStorageVersionCurrent || physicalLossSeries != 0 {
		t.Fatalf("migrated version=%d loss_series=%d", userVersion, physicalLossSeries)
	}
	definition, err := store.GetMetric(ctx, sqliteMergedPingLatencyMetric)
	if err != nil || definition.RetentionDays != 19 {
		t.Fatalf("migration changed retention: definition=%#v err=%v", definition, err)
	}
	loss, err := store.Query(ctx, Query{MetricName: sqliteVirtualPingLossMetric, EntityID: "node-a", Start: base, End: base.Add(time.Minute)})
	if err != nil || len(loss) != 2 || loss[0].Value != 0 || loss[1].Value != 1 {
		t.Fatalf("migrated virtual loss=%#v err=%v", loss, err)
	}
}

func TestSQLiteV8MigrationMismatchRollsBackPingMerge(t *testing.T) {
	ctx := context.Background()
	dsn := sqliteFileDSN(filepath.Join(t.TempDir(), "metrics.db"))
	legacy := createSQLiteV8LegacyStore(t, ctx, dsn)
	base := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	for _, name := range []string{sqliteMergedPingLatencyMetric, sqliteVirtualPingLossMetric} {
		if err := legacy.CreateMetric(ctx, Definition{Name: name, Type: TypeGauge, RetentionDays: 7}); err != nil {
			t.Fatal(err)
		}
	}
	if err := legacy.WriteBatch(ctx, []Point{
		{MetricName: sqliteMergedPingLatencyMetric, EntityID: "node-a", Timestamp: base, Value: 20},
		{MetricName: sqliteMergedPingLatencyMetric, EntityID: "node-a", Timestamp: base.Add(5 * time.Second), Value: -1},
		{MetricName: sqliteVirtualPingLossMetric, EntityID: "node-a", Timestamp: base, Value: 0},
	}); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	if migrated, err := Open(ctx, SQLite(dsn)); err == nil {
		_ = migrated.Close()
		t.Fatal("mismatched Ping series unexpectedly migrated")
	} else if !strings.Contains(err.Error(), "ping point count mismatch") {
		t.Fatalf("unexpected migration error: %v", err)
	}
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var userVersion, latencySeries, lossSeries int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&userVersion); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM metric_series WHERE metric_name = ?`, sqliteMergedPingLatencyMetric).Scan(&latencySeries); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM metric_series WHERE metric_name = ?`, sqliteVirtualPingLossMetric).Scan(&lossSeries); err != nil {
		t.Fatal(err)
	}
	if userVersion >= sqliteStorageVersionPingMerge || latencySeries != 1 || lossSeries != 1 {
		t.Fatalf("failed migration changed Ping data: version=%d latency=%d loss=%d", userVersion, latencySeries, lossSeries)
	}
}

func TestSQLiteV8MigratesLegacyPingRollupsWithoutRawPoints(t *testing.T) {
	ctx := context.Background()
	dsn := sqliteFileDSN(filepath.Join(t.TempDir(), "metrics.db"))
	legacy := createSQLiteV8LegacyStore(t, ctx, dsn)
	for _, name := range []string{sqliteMergedPingLatencyMetric, sqliteVirtualPingLossMetric} {
		if err := legacy.CreateMetric(ctx, Definition{Name: name, Type: TypeGauge, RetentionDays: 7}); err != nil {
			t.Fatal(err)
		}
	}
	bucket := time.Now().UTC().Add(-time.Hour).Truncate(time.Minute)
	if err := insertSQLiteV8LegacyPingRollups(ctx, legacy, bucket, 2); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := Open(ctx, SQLite(dsn, WithRollupPolicy(RollupPolicy{
		RawRetention: 15 * time.Minute,
		Tiers:        []RollupTier{{Interval: time.Minute, Retention: 24 * time.Hour}},
	})))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	rows, err := store.scanRollupRowsBetween(ctx, sqliteMergedPingLatencyMetric, "node-a", map[string]string{"task_id": "task-a"},
		time.Minute.Nanoseconds(), bucket.UnixNano(), bucket.UnixNano(), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("migrated latency rollups=%d want=1", len(rows))
	}
	got := rows[0].bucketData
	if got.count != 4 || got.lossCount != 2 || got.sum != 40 || got.sumSq != 1000 || got.min != 10 || got.max != 30 {
		t.Fatalf("migrated latency rollup changed: %#v", got)
	}
	if value, ok := got.value(aggPingLossAvg); !ok || value != 0.5 {
		t.Fatalf("migrated virtual loss average=%v ok=%v", value, ok)
	}
	var lossSeries int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM metric_series WHERE metric_name = ?`, sqliteVirtualPingLossMetric).Scan(&lossSeries); err != nil {
		t.Fatal(err)
	}
	if lossSeries != 0 {
		t.Fatalf("legacy rollup migration retained %d physical loss series", lossSeries)
	}
}

func TestSQLiteV8LegacyPingRollupMismatchRollsBack(t *testing.T) {
	ctx := context.Background()
	dsn := sqliteFileDSN(filepath.Join(t.TempDir(), "metrics.db"))
	legacy := createSQLiteV8LegacyStore(t, ctx, dsn)
	for _, name := range []string{sqliteMergedPingLatencyMetric, sqliteVirtualPingLossMetric} {
		if err := legacy.CreateMetric(ctx, Definition{Name: name, Type: TypeGauge, RetentionDays: 7}); err != nil {
			t.Fatal(err)
		}
	}
	if err := insertSQLiteV8LegacyPingRollups(ctx, legacy, time.Now().UTC().Add(-time.Hour).Truncate(time.Minute), 3); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	if migrated, err := Open(ctx, SQLite(dsn)); err == nil {
		_ = migrated.Close()
		t.Fatal("inconsistent legacy Ping rollup unexpectedly migrated")
	} else if !strings.Contains(err.Error(), "loss summary square count is inconsistent") {
		t.Fatalf("unexpected migration error: %v", err)
	}
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var userVersion, lossSeries int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&userVersion); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM metric_series WHERE metric_name = ?`, sqliteVirtualPingLossMetric).Scan(&lossSeries); err != nil {
		t.Fatal(err)
	}
	if userVersion >= sqliteStorageVersionPingMerge || lossSeries != 1 {
		t.Fatalf("failed rollup migration changed storage: version=%d loss_series=%d", userVersion, lossSeries)
	}
}

func TestSQLiteV8PingLossCountSurvivesTierHandoff(t *testing.T) {
	ctx := context.Background()
	policy := RollupPolicy{
		RawRetention: 10 * time.Minute,
		Tiers: []RollupTier{
			{Interval: time.Minute, Retention: 20 * time.Minute},
			{Interval: 5 * time.Minute, Retention: 2 * time.Hour},
			{Interval: time.Hour, Retention: 24 * time.Hour},
		},
	}
	store, err := Open(ctx, SQLiteInDir(t.TempDir(), WithRollupPolicy(policy)))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, definition := range []Definition{
		{Name: sqliteMergedPingLatencyMetric, Type: TypeGauge, RetentionDays: 1},
		{Name: sqliteVirtualPingLossMetric, Type: TypeGauge, RetentionDays: 1},
	} {
		if err := store.CreateMetric(ctx, definition); err != nil {
			t.Fatal(err)
		}
	}

	now := time.Now().UTC().Truncate(time.Minute)
	base := now.Add(-90 * time.Minute).Truncate(5 * time.Minute).Add(time.Minute)
	latencies := []float64{10, -1, 30, -1}
	points := make([]Point, 0, len(latencies)*2)
	for index, latency := range latencies {
		timestamp := base.Add(time.Duration(index) * 5 * time.Second)
		loss := 0.0
		if latency < 0 {
			loss = 1
		}
		points = append(points,
			Point{MetricName: sqliteMergedPingLatencyMetric, EntityID: "node-a", Timestamp: timestamp, Value: latency, Tags: map[string]string{"task_id": "task-a"}},
			Point{MetricName: sqliteVirtualPingLossMetric, EntityID: "node-a", Timestamp: timestamp, Value: loss, Tags: map[string]string{"task_id": "task-a"}},
		)
	}
	if err := store.WriteBatch(ctx, points); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Compact(ctx, now); err != nil {
		t.Fatal(err)
	}

	var minuteBuckets, fiveMinuteBuckets int
	if err := store.db.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM metric_rollup_blocks b JOIN metric_series s ON s.id = b.series_id WHERE s.metric_name = ? AND b.resolution_nano = ?) +
		(SELECT COUNT(*) FROM metric_rollup_values v JOIN metric_series s ON s.id = v.series_id WHERE s.metric_name = ? AND v.resolution_nano = ?)`,
		sqliteMergedPingLatencyMetric, time.Minute.Nanoseconds(), sqliteMergedPingLatencyMetric, time.Minute.Nanoseconds()).Scan(&minuteBuckets); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM metric_rollup_blocks b JOIN metric_series s ON s.id = b.series_id WHERE s.metric_name = ? AND b.resolution_nano = ?) +
		(SELECT COUNT(*) FROM metric_rollup_values v JOIN metric_series s ON s.id = v.series_id WHERE s.metric_name = ? AND v.resolution_nano = ?)`,
		sqliteMergedPingLatencyMetric, (5 * time.Minute).Nanoseconds(), sqliteMergedPingLatencyMetric, (5 * time.Minute).Nanoseconds()).Scan(&fiveMinuteBuckets); err != nil {
		t.Fatal(err)
	}
	if minuteBuckets != 0 || fiveMinuteBuckets == 0 {
		t.Fatalf("tier handoff retained duplicate buckets: one_minute=%d five_minute=%d", minuteBuckets, fiveMinuteBuckets)
	}

	seriesQuery := AggregateQuery{
		Query:       Query{EntityID: "node-a", Start: base.Truncate(5 * time.Minute), End: base.Add(5 * time.Minute), Tags: map[string]string{"task_id": "task-a"}},
		Aggregation: AggAvg,
		Interval:    5 * time.Minute,
	}
	seriesQuery.MetricName = sqliteMergedPingLatencyMetric
	latency, err := store.Series(ctx, seriesQuery, now)
	if err != nil || len(latency) != 1 || latency[0].Value != 20 {
		t.Fatalf("handoff latency=%#v err=%v", latency, err)
	}
	seriesQuery.MetricName = sqliteVirtualPingLossMetric
	loss, err := store.Series(ctx, seriesQuery, now)
	if err != nil || len(loss) != 1 || loss[0].Value != 0.5 || loss[0].MetricName != sqliteVirtualPingLossMetric {
		t.Fatalf("handoff loss=%#v err=%v", loss, err)
	}
}

func TestSQLiteV8AllLossPingHandoffAcceptsAnySamplingInterval(t *testing.T) {
	for _, test := range []struct {
		name     string
		count    int
		interval time.Duration
	}{
		{name: "five-second-probe", count: 12, interval: 5 * time.Second},
		{name: "one-second-probe", count: 60, interval: time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			policy := RollupPolicy{
				RawRetention: 10 * time.Minute,
				Tiers: []RollupTier{
					{Interval: time.Minute, Retention: 20 * time.Minute},
					{Interval: 5 * time.Minute, Retention: 2 * time.Hour},
				},
			}
			store, err := Open(ctx, SQLiteInDir(t.TempDir(), WithRollupPolicy(policy)))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			for _, definition := range []Definition{
				{Name: sqliteMergedPingLatencyMetric, Type: TypeGauge, RetentionDays: 1},
				{Name: sqliteVirtualPingLossMetric, Type: TypeGauge, RetentionDays: 1},
			} {
				if err := store.CreateMetric(ctx, definition); err != nil {
					t.Fatal(err)
				}
			}

			now := time.Now().UTC().Truncate(time.Minute)
			base := now.Add(-90 * time.Minute).Truncate(5 * time.Minute).Add(time.Minute)
			points := make([]Point, 0, test.count*2)
			for index := 0; index < test.count; index++ {
				timestamp := base.Add(time.Duration(index) * test.interval)
				points = append(points,
					Point{MetricName: sqliteMergedPingLatencyMetric, EntityID: "node-a", Timestamp: timestamp, Value: -1, Tags: map[string]string{"task_id": "task-a"}},
					Point{MetricName: sqliteVirtualPingLossMetric, EntityID: "node-a", Timestamp: timestamp, Value: 1, Tags: map[string]string{"task_id": "task-a"}},
				)
			}
			if err := store.WriteBatch(ctx, points); err != nil {
				t.Fatal(err)
			}
			if _, err := store.Compact(ctx, now); err != nil {
				t.Fatalf("all-loss handoff was blocked: %v", err)
			}

			query := AggregateQuery{
				Query:       Query{EntityID: "node-a", Start: base.Truncate(5 * time.Minute), End: base.Add(5 * time.Minute), Tags: map[string]string{"task_id": "task-a"}},
				Aggregation: AggAvg,
				Interval:    5 * time.Minute,
			}
			query.MetricName = sqliteMergedPingLatencyMetric
			latency, err := store.Series(ctx, query, now)
			if err != nil || len(latency) != 1 || latency[0].Value != 0 {
				t.Fatalf("all-loss latency after handoff=%#v err=%v", latency, err)
			}
			query.MetricName = sqliteVirtualPingLossMetric
			loss, err := store.Series(ctx, query, now)
			if err != nil || len(loss) != 1 || loss[0].Value != 1 {
				t.Fatalf("all-loss rate after handoff=%#v err=%v", loss, err)
			}
			rows, err := store.scanRollupRowsBetween(ctx, sqliteMergedPingLatencyMetric, "node-a", map[string]string{"task_id": "task-a"},
				(5 * time.Minute).Nanoseconds(), base.Truncate(5*time.Minute).UnixNano(), base.Truncate(5*time.Minute).UnixNano(), true)
			if err != nil || len(rows) != 1 || rows[0].bucketData.count != int64(test.count) ||
				rows[0].bucketData.lossCount != int64(test.count) || rows[0].bucketData.digest != nil {
				t.Fatalf("all-loss coarse bucket changed: rows=%#v err=%v", rows, err)
			}
		})
	}
}
