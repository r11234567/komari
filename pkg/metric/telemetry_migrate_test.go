package metric

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestTelemetryLegacyExpiredTiersDoNotBlockRetainedImport(t *testing.T) {
	for _, sealed := range []bool{false, true} {
		kind := map[bool]string{false: "hot", true: "compressed"}[sealed]
		t.Run(kind, func(t *testing.T) {
			for _, tc := range []struct {
				name      string
				retention int
				age       int
				wantError bool
			}{
				{"expired", 30, 70, false},
				{"disabled", 0, 5, false},
				{"retained", 30, 5, true},
				{"partial_retention_boundary", 30, 30, true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					ctx := context.Background()
					s, e := telemetryFixture(t, "")
					now := time.Date(2025, 4, 15, 12, 0, 0, 0, time.UTC)
					for _, def := range []Definition{{Name: "cpu", RetentionDays: 30}, {Name: "old-tier", RetentionDays: tc.retention}, {Name: "long-retention", RetentionDays: 90}} {
						if err := s.CreateMetric(ctx, def); err != nil {
							t.Fatal(err)
						}
					}
					at := now.AddDate(0, 0, -tc.age).Truncate(24 * time.Hour)
					bucket := newRollupBucket(100)
					bucket.tagsJSON = "{}"
					bucket.addPoint(42, at.Add(13*time.Hour).UnixNano())
					tx, err := s.db.BeginTx(ctx, nil)
					if err != nil {
						t.Fatal(err)
					}
					buckets := map[rollupKey]*rollupBucket{{entityID: "node", bucket: at.UnixNano()}: bucket}
					if _, err = s.writeRollupBucketsTx(ctx, "old-tier", 24*time.Hour, buckets, tx); err != nil {
						tx.Rollback()
						t.Fatal(err)
					}
					if sealed {
						if _, err = s.sealAllSQLiteV4RollupsTx(ctx, tx, 0, 0); err != nil {
							tx.Rollback()
							t.Fatal(err)
						}
					}
					if err = tx.Commit(); err != nil {
						t.Fatal(err)
					}
					if err = s.Write(ctx, Point{MetricName: "cpu", EntityID: "node", Timestamp: now.Add(-30 * time.Minute), Value: 7}); err != nil {
						t.Fatal(err)
					}
					if _, err = s.db.Exec("INSERT INTO "+e.table("progress")+" (task,value) VALUES ('legacy_migration_now',?)", now.UnixNano()); err != nil {
						t.Fatal(err)
					}
					cfg := e.cfg
					if err = e.Close(); err != nil {
						t.Fatal(err)
					}
					err = s.MigrateLegacy(ctx, cfg, nil)
					if tc.wantError {
						if err == nil || !strings.Contains(err.Error(), "unsupported legacy interval") {
							t.Fatalf("retained incompatible tier must preserve source and refuse import: %v", err)
						}
					} else {
						if err != nil {
							t.Fatalf("obsolete tier blocked retained data: %v", err)
						}
						if err = s.MigrateLegacy(ctx, cfg, nil); err != nil {
							t.Fatal("completed import retry", err)
						}
						if err = s.EnableTelemetry(ctx, cfg); err != nil {
							t.Fatal(err)
						}
						q := AggregateQuery{Query: Query{MetricName: "cpu", EntityID: "node", Start: now.Add(-time.Hour), End: now.Add(-time.Nanosecond)}, Interval: time.Hour, Aggregation: AggSum, PreserveSeries: true}
						got, err := s.Telemetry().Chart(ctx, q, now)
						if err != nil || len(got.Points) != 1 || got.Points[0].Value != 7 || got.Points[0].Count != 1 {
							t.Fatalf("retained raw data changed: %+v %v", got, err)
						}
					}
					var sourceRows int
					if err = s.db.QueryRow("SELECT (SELECT COUNT(*) FROM metric_rollup_values)+(SELECT COUNT(*) FROM metric_rollup_blocks)").Scan(&sourceRows); err != nil || sourceRows != 1 {
						t.Fatalf("source tier was modified: rows=%d error=%v", sourceRows, err)
					}
				})
			}
		})
	}
}

func TestTelemetryLegacyNonNestedAlternateTiers(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "covered", true: "uncovered_extension"}[missing], func(t *testing.T) {
			ctx := context.Background()
			s, e := telemetryFixture(t, "")
			now := time.Date(2025, 4, 15, 12, 0, 0, 0, time.UTC)
			at := now.Add(-6 * time.Hour)
			if err := s.CreateMetric(ctx, Definition{Name: "cpu", RetentionDays: 30}); err != nil {
				t.Fatal(err)
			}
			for _, resolution := range []time.Duration{time.Hour, 45 * time.Minute} {
				buckets := map[rollupKey]*rollupBucket{}
				for i := 0; i < 12; i++ {
					observed := at.Add(time.Duration(i)*15*time.Minute + time.Second)
					bucket := observed.Truncate(resolution)
					if missing && resolution == time.Hour && bucket.Equal(at.Add(time.Hour)) {
						continue
					}
					key := rollupKey{entityID: "node", bucket: bucket.UnixNano()}
					if buckets[key] == nil {
						buckets[key] = newRollupBucket(100)
						buckets[key].tagsJSON = "{}"
					}
					buckets[key].addPoint(float64(i+1), observed.UnixNano())
				}
				tx, err := s.db.BeginTx(ctx, nil)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = s.writeRollupBucketsTx(ctx, "cpu", resolution, buckets, tx); err != nil {
					tx.Rollback()
					t.Fatal(err)
				}
				if err = tx.Commit(); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.db.Exec("INSERT INTO "+e.table("progress")+"(task,value) VALUES('legacy_migration_now',?)", now.UnixNano()); err != nil {
				t.Fatal(err)
			}
			cfg := e.cfg
			if err := e.Close(); err != nil {
				t.Fatal(err)
			}
			err := s.MigrateLegacy(ctx, cfg, nil)
			if missing {
				if !errors.Is(err, ErrTelemetryCoverage) {
					t.Fatalf("unrepresented source extension accepted: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = s.EnableTelemetry(ctx, cfg); err != nil {
				t.Fatal(err)
			}
			q := AggregateQuery{Query: Query{MetricName: "cpu", EntityID: "node", Start: at, End: at.Add(3*time.Hour - time.Nanosecond)}, Interval: 3 * time.Hour, Aggregation: AggSum, PreserveSeries: true}
			got, err := s.Telemetry().Chart(ctx, q, now)
			if err != nil || len(got.Points) != 1 || got.Points[0].Value != 78 || got.Points[0].Count != 12 {
				t.Fatalf("non-nested tiers changed or duplicated population: %+v %v", got, err)
			}
		})
	}
}

func TestLegacyMigrationObservedRangeCovered(t *testing.T) {
	cell := func(series string, from int, count int64) telemetryMigrationCell {
		return telemetryMigrationCell{series: series, bucket: int64(time.Duration(from) * time.Hour), resolution: time.Hour, stats: telemetryStats{Attempts: count}}
	}
	for _, tc := range []struct {
		name        string
		cells       []telemetryMigrationCell
		first, last time.Duration
		want        bool
	}{
		{"adjacent", []telemetryMigrationCell{cell("a", 0, 1), cell("a", 1, 1)}, 45 * time.Minute, 90 * time.Minute, true},
		{"actual_tail", []telemetryMigrationCell{cell("a", 0, 1)}, 45 * time.Minute, 59 * time.Minute, true},
		{"gap", []telemetryMigrationCell{cell("a", 0, 1), cell("a", 2, 1)}, 45 * time.Minute, 90 * time.Minute, false},
		{"other_series", []telemetryMigrationCell{cell("a", 0, 1), cell("b", 1, 1)}, 45 * time.Minute, 90 * time.Minute, false},
		{"empty", []telemetryMigrationCell{cell("a", 0, 1), cell("a", 1, 0)}, 45 * time.Minute, 90 * time.Minute, false},
		{"exclusive_end", []telemetryMigrationCell{cell("a", 0, 1)}, 45 * time.Minute, time.Hour, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := legacyMigrationObservedRangeCovered(tc.cells, "a", int64(tc.first), int64(tc.last)); got != tc.want {
				t.Fatalf("coverage=%v want=%v", got, tc.want)
			}
		})
	}
}

func TestTelemetryLegacyMigrationAppliesRetainedResolution(t *testing.T) {
	for _, tc := range []struct {
		name              string
		age, source, want time.Duration
	}{
		{"recent", 30 * time.Minute, 10 * time.Second, 10 * time.Second},
		{"six_hours", 6 * time.Hour, 10 * time.Second, time.Minute},
		{"three_days", 72 * time.Hour, time.Minute, 30 * time.Minute},
		{"twenty_days", 20 * 24 * time.Hour, time.Minute, 2 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s, e := telemetryFixture(t, "")
			now := time.Date(2025, 4, 15, 12, 0, 0, 0, time.UTC)
			at := now.Add(-tc.age).Truncate(time.Hour)
			if err := s.CreateMetric(ctx, Definition{Name: "cpu", RetentionDays: 30}); err != nil {
				t.Fatal(err)
			}
			buckets := map[rollupKey]*rollupBucket{}
			for i, value := range []float64{2, 5, 9} {
				observed := at.Add(time.Duration(i) * 10 * time.Second)
				if err := s.Write(ctx, Point{MetricName: "cpu", EntityID: "node", Timestamp: observed, Value: value}); err != nil {
					t.Fatal(err)
				}
				key := rollupKey{entityID: "node", bucket: observed.Truncate(tc.source).UnixNano()}
				if buckets[key] == nil {
					buckets[key] = newRollupBucket(100)
					buckets[key].tagsJSON = "{}"
				}
				buckets[key].addPoint(value, observed.UnixNano())
			}
			tx, err := s.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.writeRollupBucketsTx(ctx, "cpu", tc.source, buckets, tx); err != nil {
				tx.Rollback()
				t.Fatal(err)
			}
			if err = tx.Commit(); err != nil {
				t.Fatal(err)
			}
			if _, err = s.db.Exec("INSERT INTO "+e.table("progress")+"(task,value) VALUES('legacy_migration_now',?)", now.UnixNano()); err != nil {
				t.Fatal(err)
			}
			cfg := e.cfg
			if err = e.Close(); err != nil {
				t.Fatal(err)
			}
			if err = s.MigrateLegacy(ctx, cfg, nil); err != nil {
				t.Fatal(err)
			}
			var finest int64
			if err = s.db.QueryRow("SELECT MIN(resolution) FROM " + e.table("summary")).Scan(&finest); err != nil || time.Duration(finest) != tc.want {
				t.Fatalf("migration deferred required retention coarsening to the online backlog: resolution=%s want=%s error=%v", time.Duration(finest), tc.want, err)
			}
			rows, err := s.db.Query("SELECT stats FROM " + e.table("summary"))
			if err != nil {
				t.Fatal(err)
			}
			for rows.Next() {
				var encoded string
				var st telemetryStats
				if err = rows.Scan(&encoded); err != nil {
					rows.Close()
					t.Fatal(err)
				}
				if err = json.Unmarshal([]byte(encoded), &st); err != nil {
					rows.Close()
					t.Fatal(err)
				}
				digest, decodeErr := DecodeTDigest(st.Digest)
				if decodeErr != nil || digest == nil || digest.count != float64(st.N) {
					rows.Close()
					t.Fatalf("coarsening aliased a mutable sketch: observations=%d digest=%+v error=%v", st.N, digest, decodeErr)
				}
			}
			if err = rows.Err(); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			if err = rows.Close(); err != nil {
				t.Fatal(err)
			}
			if err = s.EnableTelemetry(ctx, cfg); err != nil {
				t.Fatal(err)
			}
			interval := max(time.Hour, tc.want)
			from := alignTime(at, interval)
			q := AggregateQuery{Query: Query{MetricName: "cpu", EntityID: "node", Start: from, End: from.Add(interval - time.Nanosecond)}, Interval: interval, Aggregation: AggSum, PreserveSeries: true}
			got, err := s.Telemetry().Chart(ctx, q, now)
			if err != nil || len(got.Gaps) != 0 || len(got.Points) != 1 || got.Points[0].Value != 16 || got.Points[0].Count != 3 {
				t.Fatalf("coarsening changed represented population: %+v %v", got, err)
			}
		})
	}
}

func TestTelemetryLegacyPromotionExactRetentionBoundary(t *testing.T) {
	for _, tc := range []struct{ age, source, want time.Duration }{
		{time.Hour, 10 * time.Second, time.Minute},
		{24 * time.Hour, time.Minute, 5 * time.Minute},
		{2 * 24 * time.Hour, 5 * time.Minute, 30 * time.Minute},
		{7 * 24 * time.Hour, 30 * time.Minute, time.Hour},
		{15 * 24 * time.Hour, time.Hour, 2 * time.Hour},
		{60 * 24 * time.Hour, 2 * time.Hour, 6 * time.Hour},
		{180 * 24 * time.Hour, 6 * time.Hour, 12 * time.Hour},
		{365 * 24 * time.Hour, 12 * time.Hour, 24 * time.Hour},
	} {
		t.Run(tc.age.String(), func(t *testing.T) {
			_, e := telemetryFixture(t, "")
			ctx := context.Background()
			now := time.Date(2025, 4, 15, 12, 0, 0, 0, time.UTC)
			at := now.Add(-tc.age - tc.source)
			var st telemetryStats
			st.add(Point{MetricName: "cpu", Timestamp: at.Add(time.Second), Value: 7}, 1, 0)
			cell := telemetryMigrationCell{series: telemetryMigrationKey("cpu\x00node\x00{}"), tags: "{}", bucket: at.UnixNano(), resolution: tc.source, stats: st, raw: st}
			if err := e.commitLegacyCells(ctx, "exact-boundary", "cpu", "node", []telemetryMigrationCell{cell}); err != nil {
				t.Fatal(err)
			}
			if n, err := e.PromoteLegacyChunk(ctx, now, 512); err != nil || n != 1 {
				t.Fatal(n, err)
			}
			var resolution int64
			if err := e.s.db.QueryRow("SELECT MIN(resolution) FROM " + e.table("summary")).Scan(&resolution); err != nil || time.Duration(resolution) != tc.want {
				t.Fatalf("legacy retention boundary deleted source or failed to advance: resolution=%s error=%v", time.Duration(resolution), err)
			}
			from := at.Truncate(tc.want)
			got, err := e.Chart(ctx, AggregateQuery{Query: Query{MetricName: "cpu", EntityID: "node", Start: from, End: from.Add(tc.want - time.Nanosecond)}, Interval: tc.want, Aggregation: AggSum, PreserveSeries: true}, now)
			if err != nil || len(got.Gaps) != 0 || len(got.Points) != 1 || got.Points[0].Count != 1 || got.Points[0].Value != 7 {
				t.Fatalf("exact boundary changed the observation: %+v %v", got, err)
			}
			if n, err := e.PromoteLegacyChunk(ctx, now, 512); err != nil || n != 0 {
				t.Fatal("boundary replay not idempotent", n, err)
			}
		})
	}
}

func TestTelemetryLegacyPromotionMixedResolutionsAndOwners(t *testing.T) {
	s, e := telemetryFixture(t, "")
	ctx := context.Background()
	now := time.Date(2025, 4, 15, 12, 0, 0, 0, time.UTC)
	var cells []telemetryMigrationCell
	for i, tc := range []struct {
		age, resolution time.Duration
	}{
		{30 * time.Minute, 10 * time.Second},
		{6 * time.Hour, 10 * time.Second},
		{3 * 24 * time.Hour, time.Minute},
		{3 * 24 * time.Hour, 3 * time.Minute},
		{20 * 24 * time.Hour, 15 * time.Minute},
		{20 * 24 * time.Hour, 45 * time.Minute},
	} {
		at := now.Add(-tc.age).Truncate(tc.resolution)
		if tc.resolution == 3*time.Minute {
			at = at.Add(3 * time.Minute)
		} else if tc.resolution == 45*time.Minute {
			at = at.Add(-3 * time.Hour)
		}
		var st telemetryStats
		st.add(Point{MetricName: "cpu", Timestamp: at, Value: float64(i + 1)}, 1, i)
		cells = append(cells, telemetryMigrationCell{series: telemetryMigrationKey("cpu\x00node\x00{}"), tags: "{}", bucket: at.UnixNano(), resolution: tc.resolution, stats: st})
	}
	if err := e.commitLegacyCells(ctx, "mixed-tiers", "cpu", "node", cells); err != nil {
		t.Fatal(err)
	}
	other := cells[1]
	other.series = telemetryMigrationKey("cpu\x00online\x00{}")
	if err := e.commitLegacyCells(ctx, "online-owner", "cpu", "online", []telemetryMigrationCell{other}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("UPDATE "+e.table("segments")+" SET state='open' WHERE id=?", telemetryMigrationKey("online-owner")[:32]); err != nil {
		t.Fatal(err)
	}
	processed := 0
	for steps := 0; ; steps++ {
		if steps > 10 {
			t.Fatal("promotion did not converge")
		}
		n, err := e.PromoteLegacyChunk(ctx, now, 1)
		if err != nil || n > 1 {
			t.Fatal(n, err)
		}
		processed += n
		if n == 0 {
			break
		}
	}
	if processed != 5 {
		t.Fatal("eligible legacy population", processed)
	}
	rows, err := s.db.Query("SELECT b.resolution,b.stats,a.state FROM " + e.table("summary") + " b JOIN " + e.table("segments") + " a ON a.id=b.segment_id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var attempts int64
	var sum float64
	var resolutions []int64
	for rows.Next() {
		var resolution int64
		var raw, state string
		if err = rows.Scan(&resolution, &raw, &state); err != nil {
			t.Fatal(err)
		}
		var st telemetryStats
		if err = json.Unmarshal([]byte(raw), &st); err != nil {
			t.Fatal(err)
		}
		if state == "open" {
			if resolution != int64(10*time.Second) || st.Attempts != 1 || st.Sum != 2 {
				t.Fatal("online ownership changed", resolution, st)
			}
			continue
		}
		resolutions = append(resolutions, resolution)
		attempts += st.Attempts
		sum += st.Sum
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	sort.Slice(resolutions, func(i, j int) bool { return resolutions[i] < resolutions[j] })
	want := []int64{int64(10 * time.Second), int64(time.Minute), int64(30 * time.Minute), int64(2 * time.Hour), int64(6 * time.Hour)}
	if !reflect.DeepEqual(resolutions, want) || attempts != 6 || sum != 21 {
		t.Fatal("promotion changed imported statistics", resolutions, attempts, sum)
	}
}

func TestTelemetryLegacyMigrationResumesRetentionCheckpoint(t *testing.T) {
	for _, completed := range []bool{false, true} {
		t.Run(map[bool]string{false: "interrupted_import", true: "completed_old_import"}[completed], func(t *testing.T) {
			s, e := telemetryFixture(t, "")
			ctx := context.Background()
			now := time.Date(2025, 4, 15, 12, 0, 0, 0, time.UTC)
			at := now.Add(-20 * 24 * time.Hour)
			var cells []telemetryMigrationCell
			for i, value := range []float64{3, 7} {
				var st telemetryStats
				observed := at.Add(time.Duration(i) * time.Minute)
				st.add(Point{MetricName: "cpu", Timestamp: observed, Value: value}, 1, i)
				cells = append(cells, telemetryMigrationCell{series: telemetryMigrationKey("cpu\x00node\x00{}"), tags: "{}", bucket: observed.UnixNano(), resolution: time.Minute, stats: st})
			}
			if err := e.commitLegacyCells(ctx, "old-checkpoint", "cpu", "node", cells); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec("INSERT INTO "+e.table("progress")+"(task,value) VALUES('legacy_migration_now',?)", now.UnixNano()); err != nil {
				t.Fatal(err)
			}
			if completed {
				if _, err := s.db.Exec("INSERT INTO " + e.table("progress") + "(task,value) VALUES('legacy_migration_complete',1)"); err != nil {
					t.Fatal(err)
				}
			}
			cfg := e.cfg
			fail := true
			cfg.Fault = func(stage string) error {
				if fail && stage == "legacy-before-promotion-commit" {
					fail = false
					return errors.New("retention restart")
				}
				return nil
			}
			if err := e.Close(); err != nil {
				t.Fatal(err)
			}
			if err := s.MigrateLegacy(ctx, cfg, nil); err == nil {
				t.Fatal("retention checkpoint was bypassed")
			}
			var sourceCells int
			if err := s.db.QueryRow("SELECT COUNT(*) FROM "+e.table("summary")+" WHERE resolution=?", int64(time.Minute)).Scan(&sourceCells); err != nil || sourceCells != 2 {
				t.Fatalf("failed retention transaction changed source: %d %v", sourceCells, err)
			}
			if err := s.MigrateLegacy(ctx, cfg, nil); err != nil {
				t.Fatal(err)
			}
			if err := s.MigrateLegacy(ctx, cfg, nil); err != nil {
				t.Fatal("completed retention replay", err)
			}
			if err := s.EnableTelemetry(ctx, cfg); err != nil {
				t.Fatal(err)
			}
			var marker int
			if err := s.db.QueryRow("SELECT value FROM " + e.table("progress") + " WHERE task='legacy_retention_complete'").Scan(&marker); err != nil || marker != 1 {
				t.Fatal(marker, err)
			}
			got, err := s.Telemetry().Chart(ctx, AggregateQuery{Query: Query{MetricName: "cpu", EntityID: "node", Start: at, End: at.Add(2*time.Hour - time.Nanosecond)}, Interval: 2 * time.Hour, Aggregation: AggSum, PreserveSeries: true}, now)
			if err != nil || len(got.Points) != 1 || got.Points[0].Count != 2 || got.Points[0].Value != 10 || len(got.Gaps) != 0 {
				t.Fatalf("retention retry changed population: %+v %v", got, err)
			}
		})
	}
}

func TestTelemetryLegacyRawMigrationResumeAndPromotion(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := Open(ctx, SQLite(filepath.Join(dir, "metrics.db"), WithRetiredLegacyStorage(), WithSQLiteProfile(SQLiteProfileDurable)))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.CreateMetric(ctx, Definition{Name: "cpu", RetentionDays: 30}); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Add(-4 * time.Hour).Truncate(time.Hour)
	points := telemetryBatch("", at, 1, 4, 9).Points
	if err = s.WriteBatch(ctx, points); err != nil {
		t.Fatal(err)
	}
	if err = s.RequireTelemetryMigration(ctx); err == nil {
		t.Fatal("old source silently activated")
	}
	fail := true
	cfg := TelemetryConfig{ArchiveDirectory: filepath.Join(dir, "archive"), Fault: func(stage string) error {
		if fail && stage == "migration-before-window-commit" {
			fail = false
			return errors.New("power loss")
		}
		return nil
	}}
	if err = s.MigrateLegacy(ctx, cfg, nil); err == nil {
		t.Fatal("missing injected failure")
	}
	if err = s.MigrateLegacy(ctx, cfg, nil); err != nil {
		t.Fatal(err)
	}
	// The source remains usable for comparison and offline rollback.
	old, err := s.Query(ctx, Query{MetricName: "cpu", EntityID: "node", Start: at, End: at.Add(time.Hour)})
	if err != nil || len(old) != 3 {
		t.Fatal(len(old), err)
	}
	if err = s.RequireTelemetryMigration(ctx); err != nil {
		t.Fatal(err)
	}
	if err = s.EnableTelemetry(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	e := s.Telemetry()
	q := AggregateQuery{Query: Query{MetricName: "cpu", EntityID: "node", Start: at, End: at.Add(time.Hour - time.Nanosecond)}, Interval: time.Hour, Aggregation: AggSum, PreserveSeries: true}
	check := func(now time.Time) {
		t.Helper()
		r, err := e.Chart(ctx, q, now)
		if err != nil || len(r.Points) != 1 || r.Points[0].Value != 14 || r.Points[0].Count != 3 {
			t.Fatalf("migration changed statistics %+v %v", r, err)
		}
	}
	check(time.Now())
	for {
		n, err := e.PromoteLegacyChunk(ctx, at.Add(10*24*time.Hour), 512)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
	}
	for {
		n, err := e.PruneSummaries(ctx, "cpu", 30*24*time.Hour, at.Add(10*24*time.Hour), 512)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
	}
	check(at.Add(10 * 24 * time.Hour))
	out, err := e.ExportRawBatch(WithTelemetryEntities(ctx, []string{"node"}), []Query{q.Query})
	if err != nil || len(out[0]) != 3 {
		t.Fatal(out, err)
	}
}

func TestTelemetryLegacyCompressedMigration(t *testing.T) {
	for _, shared := range []bool{false, true} {
		t.Run(map[bool]string{false: "v4", true: "shared-v6"}[shared], func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			s, err := Open(ctx, SQLite(filepath.Join(dir, "metrics.db"), WithRetiredLegacyStorage(), WithSQLiteProfile(SQLiteProfileDurable)))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			at := time.Now().UTC().Add(-4 * time.Hour).Truncate(3 * time.Hour)
			points := sqliteV6TestPoints(at.UnixNano(), 1200, 42)
			encoded, err := encodeSQLiteV4Block(points)
			if err != nil {
				t.Fatal(err)
			}
			sid, err := insertSQLiteV6LegacyPointBlock(ctx, s, "cpu", encoded)
			if err != nil {
				t.Fatal(err)
			}
			if shared {
				if _, _, err = s.migrateSQLiteV6SharedPointBlocks(ctx); err != nil {
					t.Fatal(err)
				}
			}
			// A hot correction must win, including labels, with no duplicate.
			if _, err = s.db.ExecContext(ctx, "INSERT INTO metric_point_values (series_id,ts_nano,value,labels,created_at) VALUES (?,?,?,'{\"source\":\"correction\"}',0)", sid, points[100].timestamp, 9999.25); err != nil {
				t.Fatal(err)
			}
			q := Query{MetricName: "cpu", EntityID: "node", Start: at, End: at.Add(2*time.Hour - time.Nanosecond)}
			want, err := s.Query(ctx, q)
			if err != nil || len(want) != 1200 {
				t.Fatal(len(want), err)
			}
			fail := true
			cfg := TelemetryConfig{ArchiveDirectory: filepath.Join(dir, "archive"), Fault: func(stage string) error {
				if fail && stage == "migration-before-window-commit" {
					fail = false
					return errors.New("restart")
				}
				return nil
			}}
			if err = s.MigrateLegacy(ctx, cfg, nil); err == nil {
				t.Fatal("missing failure")
			}
			if err = s.MigrateLegacy(ctx, cfg, nil); err != nil {
				t.Fatal(err)
			}
			if err = s.EnableTelemetry(ctx, cfg); err != nil {
				t.Fatal(err)
			}
			got, err := s.Telemetry().ExportRawBatch(WithTelemetryEntities(ctx, []string{"node"}), []Query{q})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got[0], want) {
				t.Fatalf("compressed migration changed raw points: got=%d want=%d", len(got[0]), len(want))
			}
			var sum float64
			for _, p := range want {
				sum += p.Value
			}
			r, err := s.Telemetry().Chart(ctx, AggregateQuery{Query: q, Interval: 3 * time.Hour, Aggregation: AggSum, PreserveSeries: true}, time.Now())
			if err != nil || len(r.Points) != 1 || math.Abs(r.Points[0].Value-sum) > 1e-8*math.Abs(sum) || r.Points[0].Count != 1200 {
				t.Fatalf("compressed summary %+v %v", r, err)
			}
		})
	}
}

func TestTelemetryLegacySummaryOnlyMigration(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := Open(ctx, SQLite(filepath.Join(dir, "metrics.db"), WithRetiredLegacyStorage(), WithSQLiteProfile(SQLiteProfileDurable)))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.CreateMetric(ctx, Definition{Name: "cpu", RetentionDays: 30}); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Add(-4 * time.Hour).Truncate(3 * time.Hour)
	buckets := map[rollupKey]*rollupBucket{}
	for i := 0; i < 5; i++ {
		b := newRollupBucket(100)
		b.tagsJSON = "{}"
		b.addPoint(float64(i+1), at.Add(time.Duration(i)*time.Minute).UnixNano())
		buckets[rollupKey{entityID: "node", bucket: at.Add(time.Duration(i) * time.Minute).UnixNano()}] = b
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.writeRollupBucketsTx(ctx, "cpu", time.Minute, buckets, tx); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if _, err = s.sealAllSQLiteV4RollupsTx(ctx, tx, 0, 0); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	cfg := TelemetryConfig{ArchiveDirectory: filepath.Join(dir, "archive")}
	if err = s.MigrateLegacy(ctx, cfg, nil); err != nil {
		t.Fatal(err)
	}
	if err = s.EnableTelemetry(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	r, err := s.Telemetry().Chart(ctx, AggregateQuery{Query: Query{MetricName: "cpu", EntityID: "node", Start: at, End: at.Add(time.Hour - time.Nanosecond)}, Interval: time.Hour, Aggregation: AggSum, PreserveSeries: true}, time.Now())
	if err != nil || len(r.Points) != 1 || r.Points[0].Value != 15 || r.Points[0].Count != 5 {
		t.Fatalf("summary-only migration %+v %v", r, err)
	}
	if _, err = s.Telemetry().ExportRawBatch(WithTelemetryEntities(ctx, []string{"node"}), []Query{{MetricName: "cpu", EntityID: "node", Start: at, End: at.Add(time.Hour)}}); !errors.Is(err, ErrTelemetryCoverage) {
		t.Fatal("summary-only source returned a successful incomplete CSV", err)
	}
}

func TestTelemetryLegacyCanonicalPromotionAtomic(t *testing.T) {
	_, e := telemetryFixture(t, "")
	ctx := context.Background()
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Hour)
	sid := telemetryMigrationKey("cpu\x00node\x00{}")
	var cells []telemetryMigrationCell
	for i, v := range []float64{2, 8} {
		var st telemetryStats
		st.add(Point{MetricName: "cpu", Timestamp: at.Add(time.Duration(i) * 10 * time.Second), Value: v}, 1, i)
		cells = append(cells, telemetryMigrationCell{series: sid, tags: "{}", bucket: at.Add(time.Duration(i) * 10 * time.Second).UnixNano(), resolution: 10 * time.Second, stats: st})
	}
	if err := e.commitLegacyCells(ctx, "canonical", "cpu", "node", cells); err != nil {
		t.Fatal(err)
	}
	e.cfg.Fault = func(stage string) error {
		if stage == "legacy-before-promotion-commit" {
			return errors.New("power loss")
		}
		return nil
	}
	if _, err := e.PromoteLegacyChunk(ctx, at.Add(4*time.Hour), 512); err == nil {
		t.Fatal("missing failure")
	}
	if n := telemetryCount(t, e, "summary"); n != 2 {
		t.Fatal("promotion lost source", n)
	}
	e.cfg.Fault = nil
	if n, err := e.PromoteLegacyChunk(ctx, at.Add(4*time.Hour), 512); err != nil || n != 2 {
		t.Fatal(n, err)
	}
	if n := telemetryCount(t, e, "summary"); n != 1 {
		t.Fatal("not merged", n)
	}
	if n, err := e.PruneSummaries(ctx, "cpu", 30*24*time.Hour, at.Add(3*time.Hour), 512); err != nil || n != 0 {
		t.Fatal("canonical prematurely pruned", n, err)
	}
}

func TestTelemetryLegacyPrecisionRecoveryNeverInventsFineHistory(t *testing.T) {
	for _, mode := range []string{"full-raw", "full-fine", "partial-fine"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			s, err := Open(ctx, SQLite(filepath.Join(dir, "metrics.db"), WithRetiredLegacyStorage(), WithSQLiteProfile(SQLiteProfileDurable)))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if err = s.CreateMetric(ctx, Definition{Name: "cpu", RetentionDays: 30}); err != nil {
				t.Fatal(err)
			}
			at := time.Now().UTC().Add(-6 * time.Hour).Truncate(time.Hour)
			var points []Point
			coarse := newRollupBucket(100)
			coarse.tagsJSON = "{}"
			fine := map[rollupKey]*rollupBucket{}
			for i := 0; i < 6; i++ {
				p := Point{MetricName: "cpu", EntityID: "node", Timestamp: at.Add(time.Duration(i) * time.Minute), Value: float64(i + 1)}
				points = append(points, p)
				coarse.addPoint(p.Value, p.Timestamp.UnixNano())
				if mode == "full-fine" || mode == "partial-fine" && i < 3 {
					b := newRollupBucket(100)
					b.tagsJSON = "{}"
					b.addPoint(p.Value, p.Timestamp.UnixNano())
					fine[rollupKey{entityID: "node", bucket: p.Timestamp.UnixNano()}] = b
				}
			}
			tx, err := s.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.writeRollupBucketsTx(ctx, "cpu", time.Hour, map[rollupKey]*rollupBucket{{entityID: "node", bucket: at.UnixNano()}: coarse}, tx); err != nil {
				t.Fatal(err)
			}
			if len(fine) > 0 {
				if _, err = s.writeRollupBucketsTx(ctx, "cpu", time.Minute, fine, tx); err != nil {
					t.Fatal(err)
				}
			}
			if err = tx.Commit(); err != nil {
				t.Fatal(err)
			}
			if mode == "full-raw" {
				if err = s.WriteBatch(ctx, points); err != nil {
					t.Fatal(err)
				}
			}
			cfg := TelemetryConfig{ArchiveDirectory: filepath.Join(dir, "archive")}
			if err = s.MigrateLegacy(ctx, cfg, nil); err != nil {
				t.Fatal(err)
			}
			if err = s.EnableTelemetry(ctx, cfg); err != nil {
				t.Fatal(err)
			}
			q := AggregateQuery{Query: Query{MetricName: "cpu", EntityID: "node", Start: at, End: at.Add(time.Hour - time.Nanosecond)}, Interval: time.Minute, Aggregation: AggSum, PreserveSeries: true}
			r, err := s.Telemetry().Chart(ctx, q, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if mode == "partial-fine" {
				if len(r.Gaps) == 0 || len(r.Points) != 0 {
					t.Fatal("fabricated partial precision", r)
				}
			} else if len(r.Points) != 6 || len(r.Gaps) != 0 {
				t.Fatal("discarded recoverable fine history", r)
			}
			q.Interval = time.Hour
			r, err = s.Telemetry().Chart(ctx, q, time.Now())
			if err != nil || len(r.Points) != 1 || r.Points[0].Count != 6 || r.Points[0].Value != 21 {
				t.Fatal("migration changed population", r, err)
			}
		})
	}
}
