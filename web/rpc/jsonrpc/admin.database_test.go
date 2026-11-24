package jsonrpc

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/komari-monitor/komari/database/metricstore"
	"github.com/komari-monitor/komari/pkg/metric"
)

func TestLocalDatabaseTotalIncludesColdArchives(t *testing.T) {
	mainSize, monitoringSize, archiveSize := int64(10), int64(15), int64(7)
	main := databaseStorageStatus{Driver: "sqlite", Location: databaseLocationLocal, Size: &mainSize}
	monitoring := databaseStorageStatus{Driver: "sqlite", Location: databaseLocationLocal, Size: &monitoringSize, ArchiveSize: &archiveSize}

	total := localDatabaseTotal(main, monitoring)
	if total == nil || *total != 32 {
		t.Fatalf("local total = %v, want 32", total)
	}

	monitoring.Location = databaseLocationExternal
	monitoring.Driver = "postgres"
	if total := localDatabaseTotal(main, monitoring); total == nil || *total != 17 {
		t.Fatalf("external SQL should leave main and local archive bytes: %v", total)
	}
	main.Files = &databaseFileSizes{Database: 5, WAL: 3, SHM: 2}
	storage := buildDashboardStorage(t.Context(), main, monitoring)
	if storage.DatabaseFiles != 12 || storage.WAL != 3 || storage.SHM != 2 {
		t.Fatalf("unexpected dashboard size breakdown: %#v", storage)
	}

	monitoring.Location = databaseLocationLocal
	monitoring.Size = nil
	if total := localDatabaseTotal(main, monitoring); total != nil {
		t.Fatalf("unknown monitoring size should not produce a local total: %d", *total)
	}
}

func TestDatabaseRuntimeStatusHidesCheckpointDetailsForExternalStores(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.FixedZone("test", 8*60*60))
	runtime := metricstore.RuntimeStatus{
		CurrentMetric:                 "cpu",
		Progress:                      8,
		Total:                         21,
		CycleStartedAt:                now,
		LastCheckpointSuccessAt:       now,
		NextCheckpointAt:              now.Add(time.Minute),
		CheckpointPending:             true,
		ConsecutiveCheckpointFailures: 2,
	}

	sqliteStatus := newDatabaseRuntimeStatus(metric.DriverSQLite, runtime)
	if !sqliteStatus.CheckpointApplicable || sqliteStatus.LastCheckpointSuccessAt == nil || sqliteStatus.NextCheckpointAt == nil {
		t.Fatalf("SQLite runtime status omitted checkpoint details: %#v", sqliteStatus)
	}
	if sqliteStatus.CycleStartedAt == nil || sqliteStatus.CycleStartedAt.Location() != time.UTC {
		t.Fatalf("runtime timestamps must be normalized to UTC: %#v", sqliteStatus.CycleStartedAt)
	}

	externalStatus := newDatabaseRuntimeStatus(metric.DriverPostgreSQL, runtime)
	if externalStatus.CheckpointApplicable || externalStatus.LastCheckpointSuccessAt != nil || externalStatus.NextCheckpointAt != nil {
		t.Fatalf("external runtime status exposed local checkpoint details: %#v", externalStatus)
	}
}

func TestDatabaseRuntimeStatusSerializesEmptyDigestHandoffsAsArray(t *testing.T) {
	status := newDatabaseRuntimeStatus(metric.DriverSQLite, metricstore.RuntimeStatus{})
	if status.DigestHandoffDeferred == nil {
		t.Fatal("empty digest handoff status must use an initialized slice")
	}
	payload, err := json.Marshal(status)
	if err != nil {
		t.Fatalf("marshal runtime status: %v", err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("decode runtime status: %v", err)
	}
	if got := string(decoded["digest_handoff_deferred"]); got != "[]" {
		t.Fatalf("empty digest handoff JSON = %s, want []", got)
	}
}

func TestDatabaseLocationForDriver(t *testing.T) {
	tests := []struct {
		driver metric.Driver
		want   string
	}{
		{driver: metric.DriverSQLite, want: databaseLocationLocal},
		{driver: metric.DriverMySQL, want: databaseLocationExternal},
		{driver: metric.DriverPostgreSQL, want: databaseLocationExternal},
		{driver: "", want: ""},
	}
	for _, test := range tests {
		if got := databaseLocationForDriver(test.driver); got != test.want {
			t.Errorf("databaseLocationForDriver(%q) = %q, want %q", test.driver, got, test.want)
		}
	}
}

func TestDatabaseMaintenanceResponsePreservesLegacyMainSizes(t *testing.T) {
	before, after := int64(100), int64(60)
	for _, test := range []struct {
		name              string
		mainSuccess       bool
		monitoringSuccess bool
		wantAllSucceeded  bool
	}{
		{name: "both succeed", mainSuccess: true, monitoringSuccess: true, wantAllSucceeded: true},
		{name: "main fails", mainSuccess: false, monitoringSuccess: true, wantAllSucceeded: false},
		{name: "monitoring fails", mainSuccess: true, monitoringSuccess: false, wantAllSucceeded: false},
		{name: "both fail", mainSuccess: false, monitoringSuccess: false, wantAllSucceeded: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := newDatabaseMaintenanceResponse(
				databaseMaintenanceResult{Before: &before, After: &after, Success: test.mainSuccess},
				databaseMaintenanceResult{Success: test.monitoringSuccess},
			)
			if response.AllSucceeded != test.wantAllSucceeded {
				t.Fatalf("all_succeeded = %t, want %t", response.AllSucceeded, test.wantAllSucceeded)
			}
			if response.Before != before || response.After != after || response.Size != after {
				t.Fatalf("legacy sizes changed: before=%d after=%d size=%d", response.Before, response.After, response.Size)
			}
		})
	}
}

func TestDatabaseFileSizesTotalIncludesRuntimeSidecars(t *testing.T) {
	files := databaseFileSizes{Database: 40, WAL: 7, SHM: 1}
	if got := files.total(); got != 48 {
		t.Fatalf("file total = %d, want 48", got)
	}
}
