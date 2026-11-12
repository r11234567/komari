package metric

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func compactFixture(t *testing.T) (*Store, *Telemetry, time.Time) {
	t.Helper()
	s, e := telemetryFixture(t, "")
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Hour)
	at := now.Add(-2 * time.Hour)
	if err := s.CreateMetric(ctx, Definition{Name: "cpu", RetentionDays: 30}); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteBatch(ctx, []Point{{MetricName: "cpu", EntityID: "node", Timestamp: at, Value: 2}, {MetricName: "cpu", EntityID: "node", Timestamp: at.Add(time.Second), Value: 8}}); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	cfg := e.cfg
	if err := s.CompleteLegacyLedgerMigration(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if err := s.MigrateLegacy(ctx, cfg, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.EnableTelemetry(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	e = s.Telemetry()
	t.Cleanup(func() { e.Close() })
	// Recreate the previous long-key representation, then let production
	// compaction rewrite every table. Canonical identity is never fabricated.
	for _, table := range []string{"summary", "hot", "members", "gaps", "erasures"} {
		if _, err := s.db.Exec("UPDATE " + e.table(table) + " SET series_id=(SELECT canonical FROM " + e.table("series_keys") + " k WHERE '@'||k.id=series_id)"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec("UPDATE " + e.table("series") + " SET id=(SELECT canonical FROM " + e.table("series_keys") + " k WHERE '@'||k.id=" + e.table("series") + ".id)"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("DELETE FROM " + e.table("series_keys")); err != nil {
		t.Fatal(err)
	}
	return s, e, at
}

func TestTelemetryOfflineCompactIncidentCopy(t *testing.T) {
	dir := os.Getenv("KOMARI_TELEMETRY_OFFLINE_COMPACT_COPY")
	if dir == "" {
		t.Skip("disposable incident copy not supplied")
	}
	if _, err := os.Stat(filepath.Join(dir, "DISPOSABLE-CAPACITY-COPY")); err != nil {
		t.Fatal(err)
	}
	archive := os.Getenv("KOMARI_TELEMETRY_OFFLINE_COMPACT_ARCHIVE")
	if archive == "" {
		t.Fatal("read-only inherited archive path required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	s, err := Open(ctx, SQLite(filepath.Join(dir, "metrics.db"), WithTablePrefix("metric_"), WithAutoMigrate(false), WithRetiredLegacyStorage(), WithSQLiteProfile(SQLiteProfileDurable)))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	e, err := s.OpenTelemetry(ctx, TelemetryConfig{ArchiveDirectory: archive})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	start := time.Now()
	var ledger int
	if err = e.s.db.QueryRow("SELECT COALESCE(MAX(value),0) FROM " + e.table("progress") + " WHERE task='legacy_ledger_complete'").Scan(&ledger); err != nil {
		t.Fatal(err)
	}
	// The inherited metrics-only incident copy cannot prove that a separate
	// business ledger was imported. Preserve its legacy tables; never forge the
	// completion marker merely to obtain a smaller capacity number.
	result, err := e.CompactSQLite(ctx, filepath.Join(dir, "replacement.db"), filepath.Join(dir, "rollback"), ledger == 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	report := map[string]any{"result": result, "elapsed_seconds": time.Since(start).Seconds(), "source_unchanged": true, "raw_archives_reencoded": false}
	b, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "result.json"), append(b, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	t.Log(string(b))
}

func TestTelemetryOfflineCompactRetiresLegacyAndPreservesQueriesAndBackup(t *testing.T) {
	s, e, at := compactFixture(t)
	ctx := context.Background()
	dir := t.TempDir()
	output := filepath.Join(dir, "compacted.db")
	backup := filepath.Join(dir, "rollback")
	before, err := e.compactStateHash(ctx, s.db)
	if err != nil {
		t.Fatal(err)
	}
	result, err := e.CompactSQLite(ctx, output, backup, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.StateSHA256 != before || result.Bytes <= 0 || len(result.SHA256) != 64 || !result.RetainBackupUntil.After(time.Now().Add(6*24*time.Hour)) {
		t.Fatal(result)
	}
	after, err := e.compactStateHash(ctx, s.db)
	if err != nil || after != before {
		t.Fatal("source mutated", err)
	}
	if err = ValidateTelemetryBackup(ctx, filepath.Join(backup, "metrics.db"), filepath.Join(backup, "telemetry")); err != nil {
		t.Fatal(err)
	}
	target, err := Open(ctx, SQLite(output, WithRetiredLegacyStorage(), WithSQLiteProfile(SQLiteProfileDurable)))
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	if target.IsVirtualMetric(sqliteVirtualPingLossMetric) != s.IsVirtualMetric(sqliteVirtualPingLossMetric) {
		t.Fatal("retirement changed virtual ping.loss semantics")
	}
	for _, name := range []string{target.tables.points, target.tables.rollups, target.tables.series, target.tables.pointBlocks, target.tables.rollupBlocks} {
		kind, err := sqliteObjectType(ctx, target.db, name)
		if err != nil || kind != "" {
			t.Fatal("retired schema recreated", name, kind, err)
		}
	}
	if err = target.EnableTelemetry(ctx, e.cfg); err != nil {
		t.Fatal(err)
	}
	engine := target.Telemetry()
	q := AggregateQuery{Query: Query{MetricName: "cpu", EntityID: "node", Start: at, End: at.Add(time.Minute - time.Nanosecond)}, Interval: time.Minute, Aggregation: AggSum, PreserveSeries: true}
	r, err := engine.Chart(ctx, q, at.Add(2*time.Hour))
	if err != nil || len(r.Points) != 1 || r.Points[0].Value != 10 || r.Points[0].Count != 2 {
		t.Fatal(r, err)
	}
	if err = engine.RollbackLegacy(ctx, filepath.Join(dir, "bad-rollback.db"), nil); err == nil || !strings.Contains(err.Error(), "retired") {
		t.Fatal("retired tables silently used for rollback", err)
	}
	if err = e.RollbackLegacy(ctx, filepath.Join(dir, "legacy-readable.db"), nil); err != nil {
		t.Fatal("original rollback no longer works", err)
	}
	var epoch, sourceEpoch int
	if err = target.db.QueryRow("SELECT value FROM " + engine.table("progress") + " WHERE task='export_epoch'").Scan(&epoch); err != nil {
		t.Fatal(err)
	}
	if err = s.db.QueryRow("SELECT value FROM " + e.table("progress") + " WHERE task='export_epoch'").Scan(&sourceEpoch); err != nil || epoch != sourceEpoch+1 {
		t.Fatal("exports not fenced", epoch, sourceEpoch, err)
	}
	// New ingestion reuses the rewritten series, instead of creating a second
	// owner for the same identity. Survives close/reopen and duplicate ACK.
	b := TelemetryBatch{Source: "node", Identity: "after-compact", Points: []Point{{MetricName: "cpu", EntityID: "node", Timestamp: time.Now().UTC(), Value: 12}}}
	if _, err = engine.Accept(ctx, b, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if receipt, err := engine.Accept(ctx, b, time.Now().UTC()); err != nil || !receipt.Duplicate {
		t.Fatal(receipt, err)
	}
	if n := telemetryCount(t, engine, "series"); n != 1 {
		t.Fatal("duplicate canonical series", n)
	}
	if err = target.Close(); err != nil {
		t.Fatal(err)
	}
	target, err = Open(ctx, SQLite(output, WithRetiredLegacyStorage(), WithSQLiteProfile(SQLiteProfileDurable)))
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	if err = target.EnableTelemetry(ctx, e.cfg); err != nil {
		t.Fatal(err)
	}
	if receipt, err := target.Telemetry().Accept(ctx, b, time.Now().UTC()); err != nil || !receipt.Duplicate {
		t.Fatal(receipt, err)
	}
}

func TestTelemetryOfflineCompactFailureNeverPublishesOrChangesSource(t *testing.T) {
	for _, stage := range []string{"compact-backup-verified", "compact-before-commit", "compact-before-publish"} {
		t.Run(stage, func(t *testing.T) {
			s, e, _ := compactFixture(t)
			before, err := e.compactStateHash(context.Background(), s.db)
			if err != nil {
				t.Fatal(err)
			}
			e.cfg.Fault = func(s string) error {
				if s == stage {
					return errors.New("simulated interruption")
				}
				return nil
			}
			dir := t.TempDir()
			output := filepath.Join(dir, "output.db")
			if _, err = e.CompactSQLite(context.Background(), output, filepath.Join(dir, "rollback"), true, nil); err == nil {
				t.Fatal("fault ignored")
			}
			if _, err = os.Stat(output); !os.IsNotExist(err) {
				t.Fatal("partial replacement published", err)
			}
			after, err := e.compactStateHash(context.Background(), s.db)
			if err != nil || after != before {
				t.Fatal("source damaged", err)
			}
			e.cfg.Fault = nil
			if _, err = e.CompactSQLite(context.Background(), output, filepath.Join(dir, "retry-rollback"), true, nil); err != nil {
				t.Fatal("retry failed", err)
			}
		})
	}
}

func TestTelemetryOfflineCompactRejectsIncompleteRetirementAndConcurrentWrite(t *testing.T) {
	ctx := context.Background()
	_, e := telemetryFixture(t, "")
	dir := t.TempDir()
	if _, err := e.CompactSQLite(ctx, filepath.Join(dir, "not-migrated.db"), filepath.Join(dir, "not-migrated-backup"), true, nil); err == nil {
		t.Fatal("incomplete migration retired")
	}
	if _, err := e.CompactSQLite(ctx, filepath.Join(dir, "keep-legacy.db"), filepath.Join(dir, "keep-legacy-backup"), false, nil); err != nil {
		t.Fatal("non-retiring compaction rejected", err)
	}
	e.cfg.Fault = func(stage string) error {
		if stage == "compact-backup-verified" {
			_, err := e.Accept(ctx, telemetryBatch("concurrent", time.Now(), 2), time.Now())
			return err
		}
		return nil
	}
	output := filepath.Join(dir, "racy.db")
	if _, err := e.CompactSQLite(ctx, output, filepath.Join(dir, "racy-backup"), false, nil); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatal("backup/source mismatch accepted", err)
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if n := telemetryCount(t, e, "receipts"); n != 1 {
		t.Fatal("concurrent write lost", n)
	}
}

func TestTelemetryOfflineCompactPreservesOrphanErasureAndRejectsBrokenDictionary(t *testing.T) {
	ctx := context.Background()
	_, e := telemetryFixture(t, "")
	dir := t.TempDir()
	canonical := telemetryMigrationKey("deleted-series")
	if _, err := e.s.db.Exec("INSERT INTO "+e.table("erasures")+" (series_id,through_id) VALUES (?,7)", canonical); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "target.db")
	if _, err := e.CompactSQLite(ctx, output, filepath.Join(dir, "rollback"), false, nil); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, SQLite(output, WithRetiredLegacyStorage(), WithSQLiteProfile(SQLiteProfileDurable)))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var actual string
	var through int
	if err = s.db.QueryRow("SELECT k.canonical,e.through_id FROM "+e.table("erasures")+" e JOIN "+e.table("series_keys")+" k ON e.series_id='@'||k.id").Scan(&actual, &through); err != nil || actual != canonical || through != 7 {
		t.Fatal(actual, through, err)
	}
	if _, err = e.s.db.Exec("INSERT INTO " + e.table("erasures") + " (series_id,through_id) VALUES ('@999999',7)"); err != nil {
		t.Fatal(err)
	}
	broken := filepath.Join(dir, "broken.db")
	if _, err = e.CompactSQLite(ctx, broken, filepath.Join(dir, "broken-backup"), false, nil); err == nil || !strings.Contains(err.Error(), "dictionary") {
		t.Fatal("broken dictionary published", err)
	}
	if _, err = os.Stat(broken); !os.IsNotExist(err) {
		t.Fatal(err)
	}
}
