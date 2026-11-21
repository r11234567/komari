package admin

import (
	"archive/zip"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/internal/passkeycredential"
	"github.com/komari-monitor/komari/internal/plugin"
	"github.com/komari-monitor/komari/web/backup"
)

func createConfigSnapshotFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "komari.db")
	db, err := openSnapshotDatabase(path)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	statements := []string{
		`CREATE TABLE configs (key TEXT PRIMARY KEY, value TEXT)`,
		`CREATE TABLE users (uuid TEXT PRIMARY KEY, username TEXT)`,
		`CREATE TABLE sessions (session TEXT PRIMARY KEY, uuid TEXT)`,
		`CREATE TABLE clients (uuid TEXT PRIMARY KEY, name TEXT)`,
		`CREATE TABLE ping_tasks (id INTEGER PRIMARY KEY, name TEXT)`,
		`CREATE TABLE return_route_tasks (id INTEGER PRIMARY KEY, name TEXT)`,
		`CREATE TABLE clipboards (id INTEGER PRIMARY KEY, text TEXT)`,
		`CREATE TABLE ping_loss_notifications (id INTEGER PRIMARY KEY, last_notified TEXT, alert_active INTEGER)`,
		`CREATE TABLE records (client TEXT, value REAL)`,
		`CREATE TABLE metric_points (metric_name TEXT, value REAL)`,
		`CREATE TABLE logs (id INTEGER PRIMARY KEY, message TEXT)`,
		`CREATE TABLE return_route_events (id INTEGER PRIMARY KEY, target TEXT)`,
		`CREATE TABLE traffic_calibration_adjustments (id INTEGER PRIMARY KEY, client TEXT, day TEXT)`,
		`CREATE TABLE telemetry_traffic_minutes (client TEXT, start INTEGER, up_bytes INTEGER)`,
		`CREATE TABLE telemetry_traffic_tails (client TEXT, observed INTEGER, up_bytes INTEGER)`,
		`INSERT INTO telemetry_traffic_minutes VALUES ('node-a', 1, 123)`,
		`INSERT INTO telemetry_traffic_tails VALUES ('node-a', 1, 123)`,
		`INSERT INTO clients VALUES ('node-a', 'Node A')`,
		`INSERT INTO users VALUES ('user-a', 'admin')`,
		`INSERT INTO sessions VALUES ('session-a', 'user-a')`,
		`INSERT INTO ping_tasks VALUES (1, 'Probe A')`,
		`INSERT INTO return_route_tasks VALUES (1, 'Route A')`,
		`INSERT INTO clipboards VALUES (1, 'secret clipboard content')`,
		`INSERT INTO ping_loss_notifications VALUES (1, '2026-08-01T00:00:00Z', 1)`,
		`INSERT INTO records VALUES ('node-a', 1)`,
		`INSERT INTO metric_points VALUES ('cpu', 2)`,
		`INSERT INTO logs VALUES (1, 'log')`,
		`INSERT INTO return_route_events VALUES (1, 'target')`,
		`INSERT INTO traffic_calibration_adjustments VALUES (1, 'node-a', '2026-08-01')`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			db.Close()
			t.Fatalf("prepare fixture with %q: %v", statement, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close fixture: %v", err)
	}
	return path
}

func TestSanitizeConfigSnapshotKeepsServersAndTasksWithoutHistory(t *testing.T) {
	path := createConfigSnapshotFixture(t)
	if err := sanitizeConfigSnapshot(context.Background(), path, backupScopeConfig); err != nil {
		t.Fatalf("sanitize snapshot: %v", err)
	}
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatalf("open sanitized snapshot: %v", err)
	}
	defer db.Close()
	for table, want := range map[string]int{
		"users": 1, "clients": 1, "ping_tasks": 1, "return_route_tasks": 1,
		"sessions": 0, "clipboards": 0, "logs": 0, "return_route_events": 0,
		"traffic_calibration_adjustments": 0,
		"telemetry_traffic_minutes":       0, "telemetry_traffic_tails": 0,
	} {
		var got int
		if err := db.QueryRow("SELECT COUNT(*) FROM " + quoteSQLiteIdentifier(table)).Scan(&got); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if got != want {
			t.Fatalf("%s count = %d, want %d", table, got, want)
		}
	}
	var lastNotified any
	var alertActive int
	if err := db.QueryRow(`SELECT last_notified, alert_active FROM ping_loss_notifications WHERE id = 1`).Scan(&lastNotified, &alertActive); err != nil {
		t.Fatalf("read reset notification state: %v", err)
	}
	if lastNotified != nil || alertActive != 0 {
		t.Fatalf("notification runtime state was retained: last=%v active=%d", lastNotified, alertActive)
	}
	for _, table := range []string{"records", "metric_points"} {
		var count int
		if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = ?`, table).Scan(&count); err != nil {
			t.Fatalf("inspect %s: %v", table, err)
		}
		if count != 0 {
			t.Fatalf("monitoring table %s remains in config snapshot", table)
		}
	}
	for key, want := range map[string]string{
		"metric_db_driver":        `"sqlite"`,
		"metric_db_dsn":           `"./data/metrics.db"`,
		"metric_migration_target": `"sqlite|./data/metrics.db"`,
	} {
		var got string
		if err := db.QueryRow(`SELECT value FROM configs WHERE key = ?`, key).Scan(&got); err != nil {
			t.Fatalf("read %s: %v", key, err)
		}
		if got != want {
			t.Fatalf("%s = %q, want %q", key, got, want)
		}
	}
}

func TestBuildBackupArchiveUsesUpstreamCompatibleRootLayout(t *testing.T) {
	content := t.TempDir()
	if err := os.WriteFile(filepath.Join(content, "komari.db"), []byte("main"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(content, "metrics.db"), []byte("history"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(content, "sample.jsonl.zst"), []byte("immutable-compressed-history"), 0o600); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "backup.zip")
	if err := buildBackupArchive(archive, content, backupScopeFull, timeForBackupTest); err != nil {
		t.Fatalf("build archive: %v", err)
	}
	reader, err := zip.OpenReader(archive)
	if err != nil {
		t.Fatalf("open archive: %v", err)
	}
	defer reader.Close()
	found := map[string]bool{}
	for _, entry := range reader.File {
		found[entry.Name] = true
		if entry.Name == "sample.jsonl.zst" && entry.Method != zip.Store {
			t.Fatal("immutable archive was recompressed")
		}
	}
	for _, name := range []string{"komari.db", "metrics.db", "komari-backup-markup"} {
		if !found[name] {
			t.Fatalf("archive missing %s: %#v", name, found)
		}
	}
}

func TestBuildConfigurationArchiveDoesNotContainMetricHistory(t *testing.T) {
	content := t.TempDir()
	if err := os.WriteFile(filepath.Join(content, "komari.db"), []byte("configuration"), 0o600); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "config.zip")
	if err := buildBackupArchive(archive, content, backupScopeConfig, timeForBackupTest); err != nil {
		t.Fatalf("build configuration archive: %v", err)
	}
	reader, err := zip.OpenReader(archive)
	if err != nil {
		t.Fatalf("open configuration archive: %v", err)
	}
	defer reader.Close()
	found := map[string]bool{}
	for _, entry := range reader.File {
		found[entry.Name] = true
	}
	if !found["komari.db"] || !found["komari-backup-markup"] {
		t.Fatalf("configuration archive is not compatible with upstream restore: %#v", found)
	}
	if found["metrics.db"] || found["metrics.db-wal"] || found["metrics.db-shm"] {
		t.Fatalf("configuration archive contains metric history: %#v", found)
	}
}

var timeForBackupTest = func() time.Time {
	return time.Unix(0, 0).UTC()
}()

func TestFullBackupIncludesPluginsAndRestoresPasskeyCredentials(t *testing.T) {
	srcData := t.TempDir()
	mustWrite := func(rel, content string, perm os.FileMode) {
		t.Helper()
		full := filepath.Join(srcData, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), perm); err != nil {
			t.Fatal(err)
		}
		_ = os.Chmod(full, perm)
	}

	// Install and approve an active ordinary plugin through the real plugin
	// lifecycle, and also stage a pre-retirement komari-passkey package + approval
	// in state.json to verify that restore keeps ordinary plugins running while
	// preventing the retired komari-passkey package and its legacy approval from
	// auto-enabling after restore.
	_ = plugin.CloseAll()
	oldDir, oldStorage := plugin.DataDir, plugin.StorageDir
	plugin.DataDir = filepath.Join(srcData, "plugin")
	plugin.StorageDir = filepath.Join(srcData, "plugin-data")
	t.Cleanup(func() {
		_ = plugin.CloseAll()
		plugin.DataDir = oldDir
		plugin.StorageDir = oldStorage
	})
	gin.SetMode(gin.TestMode)
	plugin.Init(gin.New())

	helloZip := filepath.Join(t.TempDir(), "hello-backup.zip")
	if err := os.WriteFile(helloZip, admissionArchiveDetails(t, "hello-backup", "0.1.0", `{"node":true,"timeout":5}`, `function load() {}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := plugin.InstallZip(helloZip); err != nil {
		t.Fatalf("install hello-backup fixture: %v", err)
	}
	if err := plugin.SetEnabled("hello-backup", true, true); err != nil {
		t.Fatalf("enable hello-backup fixture: %v", err)
	}
	if err := plugin.CloseAll(); err != nil {
		t.Fatalf("close source plugins before backup: %v", err)
	}

	// Simulate an old backup archive that still had komari-passkey installed,
	// enabled, and approved in plugin/state.json.
	mustWrite("plugin/komari-passkey/komari-plugin.json", `{"name":"Legacy Passkey","short":"komari-passkey","version":"0.1.14","komari":">=0.0.1","entry":"script.js","permissions":{"node":true,"timeout":5}}`, 0o644)
	mustWrite("plugin/komari-passkey/script.js", `function load() {}`, 0o644)
	mustWrite("plugin/komari-passkey/.komari-package.json", `{"digest":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}`, 0o600)
	statePath := filepath.Join(srcData, "plugin", "state.json")
	var stateDoc map[string]map[string]map[string]any
	if rawState, err := os.ReadFile(statePath); err != nil || json.Unmarshal(rawState, &stateDoc) != nil {
		t.Fatalf("read source plugin state.json: %v", err)
	}
	stateDoc["plugins"]["komari-passkey"] = map[string]any{
		"enabled":                   true,
		"approved_permissions_hash": "legacy-approved-hash",
		"approved_package_digest":   "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	}
	mergedState, err := json.MarshalIndent(stateDoc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, mergedState, 0o600); err != nil {
		t.Fatal(err)
	}

	userUUID := "user-backup-passkey-1"
	credID := base64.RawURLEncoding.EncodeToString([]byte("passkey-cred-id-1"))
	pubKey := base64.RawURLEncoding.EncodeToString([]byte("synthetic-cbor-public-key-bytes"))
	passkeyJSON, err := json.Marshal(map[string]any{
		"version": 1,
		"credentials": []map[string]any{{
			"userUUID":   userUUID,
			"id":         credID,
			"publicKey":  pubKey,
			"counter":    7,
			"deviceType": "singleDevice",
			"backedUp":   false,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	mustWrite("theme/custom/theme.json", `{"name":"custom"}`, 0o644)
	mustWrite("plugin-data/komari-passkey/passkeys.json", string(passkeyJSON), 0o600)

	// Configuration-only backup retains its separate scope without plugin runtime/state.
	configContent := t.TempDir()
	if err := copyPersistentFilesFrom(srcData, configContent, backupScopeConfig); err != nil {
		t.Fatalf("copy config persistent files: %v", err)
	}
	if _, err := os.Stat(filepath.Join(configContent, "theme", "custom", "theme.json")); err != nil {
		t.Fatalf("config backup missing theme: %v", err)
	}
	for _, excluded := range []string{"plugin", "plugin-data"} {
		if _, err := os.Stat(filepath.Join(configContent, excluded)); !os.IsNotExist(err) {
			t.Fatalf("config backup unexpectedly included %s", excluded)
		}
	}

	// Build a complete backup archive with a REAL SQLite komari.db so it goes
	// through the exact startup restore validation and atomic directory swap.
	fullContent := t.TempDir()
	if err := copyPersistentFilesFrom(srcData, fullContent, backupScopeFull); err != nil {
		t.Fatalf("copy full persistent files: %v", err)
	}
	realDB := createConfigSnapshotFixture(t)
	if err := copyFile(realDB, filepath.Join(fullContent, "komari.db"), 0o600); err != nil {
		t.Fatalf("stage real komari.db: %v", err)
	}

	targetRoot := t.TempDir()
	restoredData := filepath.Join(targetRoot, "data")
	if err := os.MkdirAll(restoredData, 0o755); err != nil {
		t.Fatal(err)
	}
	oldDB := createConfigSnapshotFixture(t)
	if err := copyFile(oldDB, filepath.Join(restoredData, "komari.db"), 0o600); err != nil {
		t.Fatalf("seed previous komari.db: %v", err)
	}
	stagedBackupZip := filepath.Join(restoredData, "backup.zip")
	if err := buildBackupArchive(stagedBackupZip, fullContent, backupScopeFull, timeForBackupTest); err != nil {
		t.Fatalf("build full archive: %v", err)
	}
	if err := backup.ValidateArchive(stagedBackupZip); err != nil {
		t.Fatalf("full archive with plugins failed validation: %v", err)
	}

	// Run the REAL startup restore path (SQLite header validation + unzip + atomic swap).
	if err := dbcore.RestoreStagedBackup(restoredData); err != nil {
		t.Fatalf("real startup restore failed: %v", err)
	}

	restoredPasskeyPath := filepath.Join(restoredData, "plugin-data", "komari-passkey", "passkeys.json")
	info, err := os.Stat(restoredPasskeyPath)
	if err != nil {
		t.Fatalf("restored passkeys.json missing: %v", err)
	}
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("restored passkeys.json perm = %04o, want 0600", perm)
		}
	}

	// Boot plugins from the restored directory using plugin.LoadAll() ONLY (no
	// SetEnabled bypass) to prove persisted enabled state and approvals survive
	// restore for active plugins while retired komari-passkey remains disabled.
	plugin.DataDir = filepath.Join(restoredData, "plugin")
	plugin.StorageDir = filepath.Join(restoredData, "plugin-data")
	plugin.Init(gin.New())
	if err := plugin.LoadAll(); err != nil {
		t.Fatalf("plugin.LoadAll after restore failed: %v", err)
	}

	var helloRunning, legacyRunning, legacyEnabled bool
	for _, item := range plugin.List() {
		switch item.Short {
		case "hello-backup":
			helloRunning = item.Enabled && item.Running
		case "komari-passkey":
			legacyEnabled = item.Enabled
			legacyRunning = item.Running
		}
	}
	if !helloRunning {
		t.Fatal("restored hello-backup plugin is not running after plugin.LoadAll()")
	}
	if legacyEnabled || legacyRunning {
		t.Fatalf("retired komari-passkey plugin was re-enabled after restore (enabled=%v running=%v)", legacyEnabled, legacyRunning)
	}
	if err := plugin.SetEnabled("komari-passkey", true, true); err == nil {
		t.Fatal("expected enabling retired komari-passkey plugin to fail")
	}

	snapshot, err := passkeycredential.Read(context.Background(), userUUID)
	if err != nil {
		t.Fatalf("passkeycredential.Read after restore failed: %v", err)
	}
	if snapshot.Running {
		t.Fatal("passkeycredential.Read must not report retired legacy plugin as running")
	}
}
