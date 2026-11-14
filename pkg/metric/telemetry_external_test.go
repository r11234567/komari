package metric

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Exercise the same composed contract suite even when external SQL DSNs are
// absent, including the lifecycle boundaries between independent scenarios.
func TestTelemetrySQLiteIntegration(t *testing.T) {
	telemetryExternal(t, SQLite(filepath.Join(t.TempDir(), "metrics.db"), WithRetiredLegacyStorage(), WithSQLiteProfile(SQLiteProfileDurable)))
}

func TestTelemetryPostgreSQLIntegration(t *testing.T) {
	dsn := os.Getenv("METRIC_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("METRIC_POSTGRES_DSN is not set")
	}
	telemetryExternal(t, PostgreSQL(dsn, WithRetiredLegacyStorage()))
}
func TestTelemetryMySQLIntegration(t *testing.T) {
	dsn := os.Getenv("METRIC_MYSQL_DSN")
	if dsn == "" {
		t.Skip("METRIC_MYSQL_DSN is not set")
	}
	telemetryExternal(t, MySQL(dsn, WithRetiredLegacyStorage()))
}
func TestTelemetryMariaDBIntegration(t *testing.T) {
	dsn := os.Getenv("METRIC_MARIADB_DSN")
	if dsn == "" {
		t.Skip("METRIC_MARIADB_DSN is not set")
	}
	telemetryExternal(t, MySQL(dsn, WithRetiredLegacyStorage()))
}

func telemetryExternal(t *testing.T, cfg Config) {
	t.Helper()
	t.Run("legacy_migration", func(t *testing.T) {
		ctx, e := telemetryExternalFixture(t, cfg)
		s, archive := e.s, e.cfg
		if err := e.Close(); err != nil {
			t.Fatal(err)
		}
		if err := s.CreateMetric(ctx, Definition{Name: "migration-cpu", RetentionDays: 30}); err != nil {
			t.Fatal(err)
		}
		at := time.Now().UTC().Truncate(time.Hour).Add(-time.Hour)
		batch := telemetryBatch("", at, 2, 5, 9)
		for i := range batch.Points {
			batch.Points[i].MetricName = "migration-cpu"
		}
		if err := s.WriteBatch(ctx, batch.Points); err != nil {
			t.Fatal(err)
		}
		// Exercise the retained-tier selection SQL on actual legacy rows, not
		// only an empty rollup table. Expired daily history must not block
		// importing an otherwise supported hourly population on any driver.
		if err := s.CreateMetric(ctx, Definition{Name: "migration-expired", RetentionDays: 1}); err != nil {
			t.Fatal(err)
		}
		for _, tier := range []struct {
			metric string
			at     time.Time
			step   time.Duration
			values []float64
		}{
			{"migration-cpu", at, time.Hour, []float64{2, 5, 9}},
			{"migration-expired", at.Add(-5 * 24 * time.Hour).Truncate(24 * time.Hour), 24 * time.Hour, []float64{42}},
		} {
			bucket := newRollupBucket(100)
			bucket.tagsJSON = `{"device":"0"}`
			for i, value := range tier.values {
				bucket.addPoint(value, tier.at.Add(time.Duration(i)*time.Millisecond).UnixNano())
			}
			tx, err := s.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.writeRollupBucketsTx(ctx, tier.metric, tier.step, map[rollupKey]*rollupBucket{{entityID: "node", bucket: tier.at.UnixNano()}: bucket}, tx); err != nil {
				tx.Rollback()
				t.Fatal(err)
			}
			if err = tx.Commit(); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.MigrateLegacy(ctx, archive, nil); err != nil {
			t.Fatal("legacy driver migration", err)
		}
		if err := s.EnableTelemetry(ctx, archive); err != nil {
			t.Fatal(err)
		}
		defer s.Telemetry().Close()
		q := Query{MetricName: "migration-cpu", EntityID: "node", Start: at, End: at.Add(time.Minute)}
		got, err := s.Telemetry().ExportRawBatch(WithTelemetryEntities(ctx, []string{"node"}), []Query{q})
		if err != nil || len(got) != 1 || len(got[0]) != 3 || got[0][0].Value != 2 || got[0][1].Value != 5 || got[0][2].Value != 9 {
			t.Fatalf("legacy driver raw population changed: %+v %v", got, err)
		}
		chart, err := s.Telemetry().Chart(ctx, AggregateQuery{Query: Query{MetricName: "migration-cpu", EntityID: "node", Start: at, End: at.Add(time.Hour - time.Nanosecond)}, Interval: time.Hour, Aggregation: AggSum, PreserveSeries: true}, time.Now().UTC())
		if err != nil || len(chart.Gaps) != 0 || len(chart.Points) != 1 || chart.Points[0].Count != 3 || chart.Points[0].Value != 16 {
			t.Fatalf("legacy driver rollup population changed: %+v %v", chart, err)
		}
	})
	t.Run("storage_contracts", func(t *testing.T) {
		ctx, e := telemetryExternalFixture(t, cfg)
		testTelemetryExternalStorage(t, ctx, e)
	})
	t.Run("packed_gaps_restart_and_exports", func(t *testing.T) {
		_, e := telemetryExternalFixture(t, cfg)
		testTelemetryPackedGapExports(t, e, time.Now().UTC().Truncate(time.Hour))
	})
	t.Run("mixed_historical_parent_with_exact_hot_edge", func(t *testing.T) {
		_, e := telemetryExternalFixture(t, cfg)
		testScalarExternalBoundary(t, e)
	})
	t.Run("half_day_and_daily_retention_handoff", func(t *testing.T) {
		ctx, e := telemetryExternalFixture(t, cfg)
		at := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
		if _, err := e.Accept(ctx, telemetryBatch("daily-tier-driver", at, 2, 5, 9), at); err != nil {
			t.Fatal(err)
		}
		summarizeAll(t, e)
		if _, err := e.SealDue(ctx, at.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		now := at.Add(400 * 24 * time.Hour)
		promoteTelemetryAll(t, e, now)
		for {
			n, err := e.PruneSummaries(ctx, "cpu", 500*24*time.Hour, now, 2)
			if err != nil {
				t.Fatal(err)
			}
			if n == 0 {
				break
			}
		}
		if n := telemetryCount(t, e, "rollup"); n != 1 {
			t.Fatal("fine tiers retained", n)
		}
		q := AggregateQuery{Query: Query{MetricName: "cpu", EntityID: "node", Start: at, End: at.Add(24*time.Hour - time.Nanosecond)}, Interval: 24 * time.Hour, Aggregation: AggSum, PreserveSeries: true}
		r, err := e.Chart(ctx, q, now)
		if err != nil || len(r.Gaps) != 0 || len(r.Points) != 1 || r.Points[0].Value != 16 || r.Points[0].Count != 3 {
			t.Fatal("daily SQL population", r, err)
		}
	})
}

// Scenarios that close/reopen the engine or deliberately close the archive
// root own their store, table namespace and archive directory independently.
func telemetryExternalFixture(t *testing.T, cfg Config) (context.Context, *Telemetry) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	cfg.TablePrefix = fmt.Sprintf("te_%d_", time.Now().UnixNano())
	s, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if cfg.Driver != DriverSQLite {
		t.Cleanup(func() { dropIntegrationTables(t, s, cfg.TablePrefix) })
	}
	// SQLite's entire database belongs to the parent TempDir, including legacy
	// compatibility views that the external engines do not create.
	e, err := s.OpenTelemetry(ctx, TelemetryConfig{ArchiveDirectory: t.TempDir(), SegmentDuration: 15 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, table := range []string{"schedule", "gaps", "traffic_ingress", "identity", "erasures", "members", "work", "leases", "outbox", "summary", "hot", "latest", "series", "receipts", "segments", "progress"} {
			if _, err := s.db.ExecContext(context.Background(), "DROP TABLE IF EXISTS "+e.table(table)); err != nil {
				t.Errorf("drop %s: %v", table, err)
			}
		}
	})
	t.Cleanup(func() { e.Close() })
	return ctx, e
}

func testTelemetryExternalStorage(t *testing.T, ctx context.Context, e *Telemetry) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Hour)
	b := telemetryBatch("sql/one", now, 1, 3, 8)
	r, err := e.Accept(ctx, b, now)
	if err != nil || r.IngestID != 1 {
		t.Fatalf("accept %+v %v", r, err)
	}
	r, err = e.Accept(ctx, b, now.Add(time.Second))
	if err != nil || !r.Duplicate {
		t.Fatalf("retry %+v %v", r, err)
	}
	if points, err := e.ReadHot(ctx, Query{MetricName: "cpu", EntityID: "node", Start: now, End: now.Add(time.Minute), Tags: map[string]string{"device": "0"}}, now); err != nil || len(points) != 3 {
		t.Fatalf("hot %+v %v", points, err)
	}
	summarizeAll(t, e)
	q := AggregateQuery{Query: Query{MetricName: "cpu", EntityID: "node", Start: now, End: now.Add(5*time.Minute - time.Nanosecond)}, Interval: 5 * time.Minute, Aggregation: AggSum, PreserveSeries: true}
	if got, err := e.Aggregate(ctx, q, now); err != nil || len(got.Points) != 1 || got.Points[0].Value != 12 {
		t.Fatalf("summary %+v %v", got, err)
	}
	if _, err = e.SealDue(ctx, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	promoteTelemetryAll(t, e, now.Add(time.Hour))
	beforeStats, _ := statsSemanticHash(t, e)
	statsCursor := ""
	for {
		n, next, compactErr := e.CompactStatsChunk(ctx, statsCursor, 2)
		if compactErr != nil {
			t.Fatal("external SQL statistics compaction", compactErr)
		}
		statsCursor = next
		if n == 0 {
			break
		}
	}
	if afterStats, _ := statsSemanticHash(t, e); beforeStats != afterStats {
		t.Fatal("external SQL compaction changed statistics")
	}
	if n, err := e.PruneSummaries(ctx, "cpu", 30*24*time.Hour, now.Add(4*time.Hour), 512); err != nil || n != 0 {
		t.Fatal("external tier handoff/GC", n, err)
	}
	if ok, err := e.ArchiveNext(ctx); err != nil || !ok {
		t.Fatalf("archive %v %v", ok, err)
	}
	if n, err := e.DeleteArchivedChunk(ctx, now.Add(time.Hour), 512); err != nil || n != 3 {
		t.Fatalf("delete %d %v", n, err)
	}
	if got, err := e.Aggregate(ctx, q, now); err != nil || len(got.Points) != 1 || got.Points[0].Value != 12 {
		t.Fatalf("cold chart %+v %v", got, err)
	}
	// Execute the actual planner SQL on every supported external engine, not
	// just a SQLite model. Older 15m and newer 5m cells must produce two 30m
	// output buckets, with the hour alternate representation counted once.
	at := now.Add(-6 * time.Hour)
	for i := 0; i < 12; i++ {
		batch := TelemetryBatch{Source: "planner", Identity: fmt.Sprintf("mixed/%d", i), Points: []Point{{MetricName: "cpu", EntityID: "planner", Timestamp: at.Add(time.Duration(i) * 5 * time.Minute), Value: float64(i + 1), Tags: map[string]string{"device": "0"}}}}
		if _, err = e.Accept(ctx, batch, now); err != nil {
			t.Fatal(err)
		}
	}
	useLegacyOnlineTiers(t, e)
	summarizeAll(t, e)
	if _, err = e.s.db.ExecContext(ctx, e.bind("DELETE FROM "+e.table("summary")+" WHERE series_id IN (SELECT id FROM "+e.table("series")+" WHERE entity=?) AND (resolution=? OR (resolution=? AND bucket<?) OR (resolution=? AND bucket>=?))"), "planner", int64(10*time.Second), int64(5*time.Minute), at.Add(30*time.Minute).UnixNano(), int64(15*time.Minute), at.Add(30*time.Minute).UnixNano()); err != nil {
		t.Fatal(err)
	}
	mixed, err := e.Chart(ctx, AggregateQuery{Query: Query{MetricName: "cpu", EntityID: "planner", Start: at, End: at.Add(time.Hour - time.Nanosecond)}, Interval: 30 * time.Minute, Aggregation: AggSum, PreserveSeries: true}, now)
	if err != nil || len(mixed.Points) != 2 || mixed.Points[0].Value != 21 || mixed.Points[1].Value != 57 || len(mixed.Gaps) != 0 {
		t.Fatalf("external mixed resolution planner: %+v %v", mixed, err)
	}
	testScalarExternalBoundary(t, e)
	if err = e.EnsureSchedule(ctx, []ScheduleTask{{Name: "summary", Due: time.Now().Add(-time.Second)}}); err != nil {
		t.Fatal(err)
	}
	_, _, ran, err := e.DispatchSchedule(ctx, time.Now(), func(context.Context, ScheduleTask) (ScheduleOutcome, error) {
		return ScheduleOutcome{Delay: time.Hour, Cursor: "cpu"}, nil
	})
	if err != nil || !ran {
		t.Fatal("external SQL scheduler", ran, err)
	}
}
