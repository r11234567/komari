package metric

import (
	"context"
	"errors"
	"math"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

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
	if _, err := e.PromoteLegacyChunk(ctx, at.Add(3*time.Hour), 512); err == nil {
		t.Fatal("missing failure")
	}
	if n := telemetryCount(t, e, "summary"); n != 2 {
		t.Fatal("promotion lost source", n)
	}
	e.cfg.Fault = nil
	if n, err := e.PromoteLegacyChunk(ctx, at.Add(3*time.Hour), 512); err != nil || n != 2 {
		t.Fatal(n, err)
	}
	if n := telemetryCount(t, e, "summary"); n != 1 {
		t.Fatal("not merged", n)
	}
	if n, err := e.PruneSummaries(ctx, "cpu", 30*24*time.Hour, at.Add(3*time.Hour), 512); err != nil || n != 0 {
		t.Fatal("canonical prematurely pruned", n, err)
	}
}
