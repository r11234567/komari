package metric

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestRetiredStartupMigrationCheckpointSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	cfg := SQLite(filepath.Join(dir, "metrics.db"), WithRetiredLegacyStorage(), WithSQLiteProfile(SQLiteProfileDurable))
	s, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.CreateMetric(ctx, Definition{Name: "cpu", RetentionDays: 30}); err != nil {
		t.Fatal(err)
	}
	if err = s.WriteBatch(ctx, telemetryBatch("", time.Now().Add(-time.Hour), 1, 2).Points); err != nil {
		t.Fatal(err)
	}
	summary, err := InspectSQLiteMigration(ctx, cfg)
	if err != nil || !summary.Required {
		t.Fatal(summary, err)
	}
	tc := TelemetryConfig{ArchiveDirectory: filepath.Join(dir, "archive")}
	first, err := s.PrepareLegacyMigration(ctx, tc)
	if err != nil {
		t.Fatal(err)
	}
	if first.Cutoff.IsZero() || first.Complete || first.LedgerComplete {
		t.Fatal(first)
	}
	if err = s.CompleteLegacyLedgerMigration(ctx, tc); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	second, err := s.PrepareLegacyMigration(ctx, tc)
	if err != nil || !second.LedgerComplete || !second.Cutoff.Equal(first.Cutoff) {
		t.Fatal(first, second, err)
	}
	summary, err = InspectSQLiteMigration(ctx, cfg)
	if err != nil || !summary.Required || summary.Layout != "telemetry-resume" {
		t.Fatal(summary, err)
	}
	if err = s.MigrateLegacy(ctx, tc, nil); err != nil {
		t.Fatal(err)
	}
	completed, err := s.PrepareLegacyMigration(ctx, tc)
	if err != nil || !completed.Complete || !completed.Cutoff.Equal(first.Cutoff) {
		t.Fatal(completed, err)
	}
	summary, err = InspectSQLiteMigration(ctx, cfg)
	if err != nil || summary.Required {
		t.Fatal(summary, err)
	}
	// Source rows remain present and a second startup must not import again.
	if err = s.MigrateLegacy(ctx, tc, func(string, int64) { t.Error("completed migration repeated") }); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyPopulationComparesRetainedScalars(t *testing.T) {
	at := time.Now()
	var a telemetryStats
	for i, v := range []float64{0, 1, 4, 5, 9, 10} {
		a.add(Point{Value: v, Timestamp: at.Add(time.Duration(i))}, 1, i)
	}
	b := a
	b.M2++
	b.Digest = nil
	b.sketch = nil
	if !equivalentLegacyPopulation(a, b) {
		t.Fatal("unretained distribution constrained scalar migration")
	}
	b.Sum++
	if equivalentLegacyPopulation(a, b) {
		t.Fatal("changed retained sum accepted")
	}
}
