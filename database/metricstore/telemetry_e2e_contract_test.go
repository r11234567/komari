package metricstore

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/komari-monitor/komari/pkg/metric"
)

// Run the actual E2E preflight against a database initialized by the production
// scheduler. Adding a task without adapting the E2E contract now fails the
// ordinary database package CI, before any remote environment or soak starts.
func TestTelemetrySchedulerMatchesE2ERetirementContract(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is required for the Linux E2E inspector contract")
	}
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "metrics.db")
	s, err := metric.Open(ctx, metric.SQLite(path, metric.WithTablePrefix("metric_"), metric.WithRetiredLegacyStorage(), metric.WithSQLiteProfile(metric.SQLiteProfileDurable)))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.CreateMetric(ctx, metric.Definition{Name: "cpu", RetentionDays: 30}); err != nil {
		t.Fatal(err)
	}
	if err = s.EnableTelemetry(ctx, metric.TelemetryConfig{ArchiveDirectory: filepath.Join(dir, "archive")}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err = s.Telemetry().Accept(ctx, metric.TelemetryBatch{Source: "node", Identity: "e2e-contract", Points: []metric.Point{{MetricName: "cpu", EntityID: "node", Timestamp: now, Value: 3}}}, now); err != nil {
		t.Fatal(err)
	}
	if n, e := s.Telemetry().SummarizeNext(ctx); e != nil || n != 1 {
		t.Fatal(n, e)
	}
	if err = ensureTelemetrySchedule(ctx, s.Telemetry(), now); err != nil {
		t.Fatal(err)
	}
	inspector, err := filepath.Abs(filepath.Join("..", "..", "scripts", "ci", "e2e", "check-telemetry-retirement.py"))
	if err != nil {
		t.Fatal(err)
	}
	const check = `import importlib.util,json,pathlib,sqlite3,sys
spec=importlib.util.spec_from_file_location("retirement",sys.argv[1])
module=importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)
with sqlite3.connect(pathlib.Path(sys.argv[2]).as_uri()+"?mode=ro",uri=True) as db:
    result=module.inspect_storage(db)
print(json.dumps(result),flush=True)
module.validate_storage(result)
`
	inspect := func(wantOK bool) map[string]any {
		t.Helper()
		out, e := exec.Command(python, "-B", "-c", check, inspector, path).CombinedOutput()
		if (e == nil) != wantOK {
			t.Fatalf("E2E preflight pass=%t, want %t: %s", e == nil, wantOK, out)
		}
		var result map[string]any
		// Diagnostics are emitted first; a failing Python assertion follows.
		if err := json.Unmarshal(bytes.SplitN(out, []byte{'\n'}, 2)[0], &result); err != nil {
			t.Fatalf("invalid E2E inspector diagnostics: %v: %s", err, out)
		}
		return result
	}
	initial := inspect(true)
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mutate := func(query string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	// Same row count must not conceal a missing responsibility.
	mutate("UPDATE metric_telemetry_schedule SET task='wrong-stats' WHERE task='stats'")
	bad := inspect(false)
	if bad["scheduled_tasks"] != initial["scheduled_tasks"] || len(bad["missing_tasks"].([]any)) != 1 || bad["missing_tasks"].([]any)[0] != "stats" {
		t.Fatal("missing task not diagnosed", bad)
	}
	mutate("UPDATE metric_telemetry_schedule SET task='stats' WHERE task='wrong-stats'")
	mutate("UPDATE metric_telemetry_schedule SET attempts=1,last_error='injected maintenance failure' WHERE task='gaps'")
	inspect(false)
	mutate("UPDATE metric_telemetry_schedule SET attempts=0 WHERE task='gaps'")
	inspect(false)
	mutate("UPDATE metric_telemetry_schedule SET attempts=0,last_error='' WHERE task='gaps'")
	inspect(true)
	// Dispatch the newly added maintenance tasks through the actual switch,
	// and confirm an idle page succeeds without triggering failure backoff.
	for _, name := range []string{"stats", "gaps"} {
		mutate("UPDATE metric_telemetry_schedule SET due=?", now.Add(time.Hour).UnixNano())
		mutate("UPDATE metric_telemetry_schedule SET due=0 WHERE task=?", name)
		if _, err = runTelemetryPass(ctx, s, now); err != nil {
			t.Fatal(name, err)
		}
		inspect(true)
	}
}
