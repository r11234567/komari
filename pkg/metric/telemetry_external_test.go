package metric

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

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
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cfg.TablePrefix = fmt.Sprintf("te_%d_", time.Now().UnixNano())
	s, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	defer dropIntegrationTables(t, s, cfg.TablePrefix)
	e, err := s.OpenTelemetry(ctx, TelemetryConfig{ArchiveDirectory: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	defer func() {
		for _, table := range []string{"schedule", "gaps", "traffic_ingress", "identity", "erasures", "members", "work", "leases", "outbox", "summary", "hot", "series", "receipts", "segments", "progress"} {
			if _, err := s.db.ExecContext(context.Background(), "DROP TABLE IF EXISTS "+e.table(table)); err != nil {
				t.Errorf("drop %s: %v", table, err)
			}
		}
	}()
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
	if ok, err := e.ArchiveNext(ctx); err != nil || !ok {
		t.Fatalf("archive %v %v", ok, err)
	}
	if n, err := e.DeleteArchivedChunk(ctx, now.Add(time.Hour), 512); err != nil || n != 3 {
		t.Fatalf("delete %d %v", n, err)
	}
	if got, err := e.Aggregate(ctx, q, now); err != nil || len(got.Points) != 1 || got.Points[0].Value != 12 {
		t.Fatalf("cold chart %+v %v", got, err)
	}
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
