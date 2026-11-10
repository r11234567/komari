package metric

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTelemetryBackupRestoreAndArchiveExpiration(t *testing.T) {
	s, e := telemetryFixture(t, "")
	ctx := context.Background()
	at := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Hour)
	if _, err := e.Accept(ctx, telemetryBatch("backup", at, 3, 7), at); err != nil {
		t.Fatal(err)
	}
	summarizeAll(t, e)
	if _, err := e.SealDue(ctx, at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if ok, err := e.ArchiveNext(ctx); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if _, err := e.DeleteArchivedChunk(ctx, time.Now(), 512); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	dbpath := filepath.Join(dir, "metrics.db")
	archive := filepath.Join(dir, "archive")
	if err := e.BackupBundle(ctx, dbpath, archive, func() error {
		// Another Store cannot expire the file while the backup is in flight.
		other, err := s.OpenTelemetry(ctx, e.cfg)
		if err != nil {
			return err
		}
		defer other.Close()
		_, err = other.ExpireArchiveNext(ctx, time.Now().Add(40*24*time.Hour), 30*24*time.Hour)
		if !errors.Is(err, ErrTelemetryBackpressure) {
			t.Fatalf("expiration crossed backup barrier: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(archive, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest telemetryBackupManifest
	if err = json.Unmarshal(data, &manifest); err != nil || len(manifest.Files) != 1 {
		t.Fatal(string(data), err)
	}
	if err = ValidateTelemetryBackup(ctx, dbpath, archive); err != nil {
		t.Fatal("valid restore refused", err)
	}
	restored, err := Open(ctx, SQLite(dbpath, WithRetiredLegacyStorage(), WithSQLiteProfile(SQLiteProfileDurable)))
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if err = restored.EnableTelemetry(ctx, TelemetryConfig{ArchiveDirectory: archive}); err != nil {
		t.Fatal(err)
	}
	q := Query{MetricName: "cpu", EntityID: "node", Start: at, End: at.Add(time.Minute)}
	points, err := restored.Telemetry().ExportRawBatch(WithTelemetryEntities(ctx, []string{"node"}), []Query{q})
	if err != nil || len(points[0]) != 2 {
		t.Fatal(points, err)
	}
	// Backup pins are released; expiration is crash-resumable and leaves charts.
	e.cfg.Fault = func(stage string) error {
		if stage == "expire-after-remove" {
			return errors.New("power loss")
		}
		return nil
	}
	if _, err = e.ExpireArchiveNext(ctx, time.Now().Add(40*24*time.Hour), 30*24*time.Hour); err == nil {
		t.Fatal("missing failure")
	}
	e.cfg.Fault = nil
	if ok, err := e.ExpireArchiveNext(ctx, time.Now().Add(40*24*time.Hour), 30*24*time.Hour); err != nil || !ok {
		t.Fatal(ok, err)
	}
	chart, err := e.Chart(ctx, AggregateQuery{Query: Query{MetricName: "cpu", EntityID: "node", Start: at, End: at.Add(time.Hour - time.Nanosecond)}, Interval: time.Hour, Aggregation: AggSum}, time.Now())
	if err != nil || len(chart.Points) != 1 || chart.Points[0].Value != 10 {
		t.Fatal(chart, err)
	}
	// Corrupt copies are refused instead of creating a successful partial bundle.
	file := filepath.Join(archive, manifest.Files[0].Path)
	if err = os.WriteFile(file, []byte("truncated"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = ValidateTelemetryBackup(ctx, dbpath, archive); err == nil {
		t.Fatal("restore accepted corrupt archive")
	}
	bad := t.TempDir()
	if err = restored.Telemetry().BackupBundle(ctx, filepath.Join(bad, "metrics.db"), filepath.Join(bad, "archive"), nil); err == nil {
		t.Fatal("corrupt archive accepted")
	}
}
