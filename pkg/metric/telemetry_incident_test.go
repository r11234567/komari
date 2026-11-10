package metric

import (
	"context"
	"database/sql"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Opt-in offline acceptance on a consistent disposable snapshot. The supplied
// source is opened read-only; neither its SQL nor adjacent archives are changed.
func TestTelemetryIncidentMigration(t *testing.T) {
	source := os.Getenv("KOMARI_TELEMETRY_INCIDENT_SOURCE")
	if source == "" {
		t.Skip("requires a local incident database, never a production connection")
	}
	ctx := context.Background()
	dir := t.TempDir()
	u := url.URL{Scheme: "file", Path: source, RawQuery: "mode=ro"}
	db, err := sql.Open("sqlite3", u.String())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "metrics.db")
	_, err = db.ExecContext(ctx, "VACUUM INTO ?", path)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, SQLite(path, WithRetiredLegacyStorage(), WithSQLiteProfile(SQLiteProfileDurable)))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var raw int64
	if err = s.db.QueryRow("SELECT (SELECT COUNT(*) FROM metric_point_values)+COALESCE((SELECT SUM(point_count) FROM metric_point_blocks),0)").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	t.Logf("Consistent incident snapshot: %d logical raw source points", raw)
	defs, err := s.ListMetrics(ctx)
	if err != nil {
		t.Fatal(err)
	}
	type expectation struct {
		q      Query
		points []Point
	}
	var checks []expectation
	for _, def := range defs {
		if len(checks) >= 6 || def.Name == sqliteVirtualPingLossMetric {
			continue
		}
		var entity string
		var ns int64
		err = s.db.QueryRow("SELECT s.entity_id,MAX(b.end_nano) FROM metric_point_blocks b JOIN metric_series s ON s.id=b.series_id WHERE s.metric_name=? GROUP BY s.entity_id LIMIT 1", def.Name).Scan(&entity, &ns)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		q := Query{MetricName: def.Name, EntityID: entity, Start: time.Unix(0, ns).Truncate(time.Hour), End: time.Unix(0, ns).Truncate(time.Hour).Add(time.Hour - time.Nanosecond)}
		points, err := s.Query(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		if len(points) > 0 && len(points) < 10000 {
			checks = append(checks, expectation{q, points})
		}
	}
	if len(checks) == 0 {
		t.Fatal("incident acceptance selected no raw parity windows")
	}
	start := time.Now()
	var imported int64
	cfg := TelemetryConfig{ArchiveDirectory: filepath.Join(dir, "archive")}
	// The supplied capture can predate today's retention horizon. Freeze this
	// disposable import at capture time, without altering source timestamps or
	// the host clock, so the acceptance actually exercises historical data.
	var captured int64
	if err = s.db.QueryRow("SELECT MAX(end_nano) FROM metric_point_blocks").Scan(&captured); err != nil {
		t.Fatal(err)
	}
	engine, err := s.OpenTelemetry(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.db.ExecContext(ctx, "INSERT INTO metric_telemetry_progress(task,value) VALUES ('legacy_migration_now',?)", captured+int64(time.Minute))
	engine.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err = s.MigrateLegacy(ctx, cfg, func(_ string, n int64) { imported += n }); err != nil {
		t.Fatal(err)
	}
	t.Logf("Migrated %d raw points in %s; rerunning completed import", imported, time.Since(start))
	if err = s.MigrateLegacy(ctx, cfg, nil); err != nil {
		t.Fatal(err)
	}
	if err = s.EnableTelemetry(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	for i, check := range checks {
		got, err := s.Telemetry().ExportRawBatch(WithTelemetryEntities(ctx, []string{check.q.EntityID}), []Query{check.q})
		if err != nil {
			t.Fatalf("parity window %d: %v", i, err)
		}
		if len(got[0]) != len(check.points) {
			t.Fatalf("parity window %d count %d/%d", i, len(got[0]), len(check.points))
		}
		for j, p := range check.points {
			v := got[0][j]
			if !p.Timestamp.Equal(v.Timestamp) || math.Float64bits(p.Value) != math.Float64bits(v.Value) {
				t.Fatalf("parity window %d point %d changed", i, j)
			}
		}
	}
	var unchanged int64
	if err = s.db.QueryRow("SELECT (SELECT COUNT(*) FROM metric_point_values)+COALESCE((SELECT SUM(point_count) FROM metric_point_blocks),0)").Scan(&unchanged); err != nil || raw != unchanged {
		t.Fatal("legacy source mutated", raw, unchanged, err)
	}
	t.Logf("Verified %d exact raw windows and preserved %d original logical raw points", len(checks), raw)
}
