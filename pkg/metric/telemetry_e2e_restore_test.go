package metric

import (
	"archive/zip"
	"context"
	"database/sql"
	"io"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The workflow calls this after a real authenticated backup download, with
// actual reports from its installed Agent. No database fixture is seeded here.
func TestTelemetryE2ERestoreBundle(t *testing.T) {
	root := os.Getenv("E2E_ROOT")
	if root == "" {
		t.Skip("requires real HTTPS/Agent workflow bundle")
	}
	if os.Getenv("GITHUB_ACTIONS") != "true" || os.Getenv("RUNNER_ENVIRONMENT") != "github-hosted" || root != filepath.Join(os.Getenv("RUNNER_TEMP"), "komari-e2e") {
		t.Fatal("not a disposable hosted fixture")
	}
	z, err := zip.OpenReader(filepath.Join(root, "real-full-backup.zip"))
	if err != nil {
		t.Fatal(err)
	}
	defer z.Close()
	dir := t.TempDir()
	target, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	for _, entry := range z.File {
		if !filepath.IsLocal(entry.Name) || entry.Mode()&os.ModeSymlink != 0 || entry.UncompressedSize64 > 4<<30 {
			t.Fatal("unsafe backup member", entry.Name)
		}
		if entry.FileInfo().IsDir() {
			if err = target.MkdirAll(entry.Name, 0700); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err = target.MkdirAll(filepath.Dir(entry.Name), 0700); err != nil {
			t.Fatal(err)
		}
		r, err := entry.Open()
		if err != nil {
			t.Fatal(err)
		}
		w, err := target.OpenFile(entry.Name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			r.Close()
			t.Fatal(err)
		}
		_, err = io.Copy(w, io.LimitReader(r, 4<<30))
		ce := w.Close()
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
		if ce != nil {
			t.Fatal(ce)
		}
	}
	ctx := context.Background()
	dbpath := filepath.Join(dir, "metrics.db")
	archive := filepath.Join(dir, "telemetry")
	if err = ValidateTelemetryBackup(ctx, dbpath, archive); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, SQLite(dbpath, WithRetiredLegacyStorage(), WithSQLiteProfile(SQLiteProfileDurable)))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.EnableTelemetry(ctx, TelemetryConfig{ArchiveDirectory: archive, RawWindow: time.Minute}); err != nil {
		t.Fatal(err)
	}
	e := s.Telemetry()
	summarizeAll(t, e)
	var entity string
	var first, last int64
	if err = s.db.QueryRow("SELECT s.entity,MIN(m.first_observed),MAX(m.last_observed) FROM metric_telemetry_members m JOIN metric_telemetry_series s ON s.id=m.series_id WHERE s.metric='cpu.usage' GROUP BY s.entity").Scan(&entity, &first, &last); err != nil {
		t.Fatal(err)
	}
	q := Query{MetricName: "cpu.usage", EntityID: entity, Start: time.Unix(0, first).Truncate(time.Hour), End: time.Unix(0, last).Truncate(time.Hour).Add(time.Hour - time.Nanosecond)}
	points, err := e.ExportRawBatch(WithTelemetryEntities(ctx, []string{entity}), []Query{q})
	if err != nil || len(points[0]) < 2 {
		t.Fatal(points, err)
	}
	var sum float64
	for _, p := range points[0] {
		sum += p.Value
	}
	chart, err := e.Chart(ctx, AggregateQuery{Query: q, Interval: time.Hour, Aggregation: AggSum, PreserveSeries: true}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var actual float64
	var count int
	for _, p := range chart.Points {
		actual += p.Value
		count += p.Count
	}
	if count != len(points[0]) || math.Abs(sum-actual) > 1e-8*math.Max(1, math.Abs(sum)) {
		t.Fatalf("restored chart/raw mismatch count=%d/%d sum=%v/%v", count, len(points[0]), actual, sum)
	}
	main, err := sql.Open("sqlite3", filepath.Join(dir, "komari.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer main.Close()
	var stream int64
	if err = main.QueryRow("SELECT through FROM telemetry_traffic_streams WHERE store_id=?", e.instance).Scan(&stream); err != nil {
		t.Fatal("missing restored traffic ownership", err)
	}
	through, err := e.progress(ctx, s.db, "ingest")
	if err != nil || stream > through {
		t.Fatal("inconsistent cross-database watermark", stream, through, err)
	}
	t.Logf("Real bundle restored: %d raw CPU points, %d chart points, traffic through %d <= accepted %d", len(points[0]), count, stream, through)
}
