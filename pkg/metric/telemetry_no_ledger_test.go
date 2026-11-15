package metric

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestTelemetryPendingIdentityHasNoHashesAndExpiresAtHandoff(t *testing.T) {
	_, e := telemetryFixture(t, "")
	ctx := context.Background()
	at := time.Now().UTC()
	b := telemetryBatch("sample/1", at, 1, 2)
	if _, err := e.Accept(ctx, b, at); err != nil {
		t.Fatal(err)
	}
	if r, err := e.Accept(ctx, b, at); err != nil || !r.Duplicate {
		t.Fatal(r, err)
	}
	var ddl string
	if err := e.s.db.QueryRow("SELECT sql FROM sqlite_master WHERE name=?", e.table("receipts")).Scan(&ddl); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ddl, "hash") || strings.Contains(ddl, "proof") {
		t.Fatal("payload ledger retained", ddl)
	}
	summarizeAll(t, e)
	if _, err := e.SealDue(ctx, at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ArchiveNext(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := e.DeleteArchivedChunk(ctx, at, 512); err != nil {
		t.Fatal(err)
	}
	if n, err := e.PruneReceipts(ctx, at, 512); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	if n := telemetryCount(t, e, "receipts"); n != 0 {
		t.Fatal(n)
	}
	var hash string
	if err := e.s.db.QueryRow("SELECT checksum FROM " + e.table("segments") + " LIMIT 1").Scan(&hash); err != nil || hash != "" {
		t.Fatal("new archive retained SHA", hash, err)
	}
	// This is deliberately best-effort telemetry. An old retry is accepted as
	// input again after handoff; it must not scan archives or retain a ledger.
	if r, err := e.Accept(ctx, b, at); err != nil || r.Duplicate {
		t.Fatal(r, err)
	}
}

func TestTelemetryRawOnlyMetricsAreArchivedWithoutChartRows(t *testing.T) {
	_, e := telemetryFixture(t, "")
	ctx := context.Background()
	at := time.Now().UTC()
	for _, name := range []string{"cpu.usage", "net.total.up", "traffic.up", "connections.tcp", "process.count"} {
		b := telemetryBatch(name, at, 10)
		b.Points[0].MetricName = name
		if _, err := e.Accept(ctx, b, at); err != nil {
			t.Fatal(err)
		}
	}
	summarizeAll(t, e)
	if n := telemetryCount(t, e, "rollup"); n != 1 {
		t.Fatal("non-chart history retained", n)
	}
	if n := telemetryCount(t, e, "hot"); n != 5 {
		t.Fatal("CSV source was dropped", n)
	}
	var ddl string
	if err := e.s.db.QueryRow("SELECT sql FROM sqlite_master WHERE name=?", e.table("rollup")).Scan(&ddl); err != nil {
		t.Fatal(err)
	}
	for _, column := range []string{"attempts ", "first_ns ", "last_ns ", "last_ingest ", "last_ordinal "} {
		if strings.Contains(ddl, column) {
			t.Fatal("redundant chart field", column)
		}
	}
}

func TestTelemetryV7RawOnlySummariesDoNotReturnAsCharts(t *testing.T) {
	s, e := telemetryFixture(t, "")
	ctx := context.Background()
	now := time.Now().UTC()
	b := telemetryBatch("old-raw-only", now.Add(-time.Hour), 10)
	b.Points[0].MetricName = "process.count"
	if _, err := e.Accept(ctx, b, now); err != nil {
		t.Fatal(err)
	}
	e.scalar = false
	summarizeAll(t, e)
	if telemetryCount(t, e, "summary") == 0 {
		t.Fatal("legacy input missing")
	}
	if _, err := s.db.Exec("UPDATE " + e.table("progress") + " SET value=7 WHERE task='schema_version'"); err != nil {
		t.Fatal(err)
	}
	cfg := e.cfg
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	target, err := s.OpenTelemetry(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	if telemetryCount(t, target, "rollup") != 0 || telemetryCount(t, target, "summary") != 0 {
		t.Fatal("raw-only legacy chart rows survived")
	}
	if telemetryCount(t, target, "hot") != 1 {
		t.Fatal("raw CSV source was removed")
	}
}
