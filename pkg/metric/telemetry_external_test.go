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
		testTelemetryPingMixedHistoricalParentWithExactHotEdge(t, e)
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
	e, err := s.OpenTelemetry(ctx, TelemetryConfig{ArchiveDirectory: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, table := range []string{"schedule", "gaps", "traffic_ingress", "identity", "erasures", "members", "work", "leases", "outbox", "summary", "hot", "series", "receipts", "segments", "progress"} {
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
	if n, err := e.PruneSummaries(ctx, "cpu", 30*24*time.Hour, now.Add(4*time.Hour), 512); err != nil || n == 0 {
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
	testTelemetryPingWindowExcludesKnownOutsideChildren(t, e)
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
