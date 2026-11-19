package dbcore

import (
	"archive/zip"
	"context"
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/komari-monitor/komari/pkg/historyexport"
	"github.com/komari-monitor/komari/pkg/metric"
)

func createRestoreSQLite(t *testing.T, path, value string) {
	t.Helper()
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE state (value TEXT); INSERT INTO state VALUES (?)`, value); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func writeRestoreArchive(t *testing.T, archivePath, databasePath string) {
	t.Helper()
	archive, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(archive)
	databaseEntry, err := writer.Create("komari.db")
	if err != nil {
		t.Fatal(err)
	}
	database, err := os.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(databaseEntry, database); err != nil {
		database.Close()
		t.Fatal(err)
	}
	database.Close()
	marker, err := writer.Create("komari-backup-markup")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := marker.Write([]byte("backup")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreStagedBackupReplacesDataOnlyAfterValidation(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	createRestoreSQLite(t, filepath.Join(dataDir, "komari.db"), "old")
	newDatabase := filepath.Join(root, "new.db")
	createRestoreSQLite(t, newDatabase, "new")
	writeRestoreArchive(t, filepath.Join(dataDir, "backup.zip"), newDatabase)

	if err := restoreStagedBackup(dataDir); err != nil {
		t.Fatalf("restore backup: %v", err)
	}
	db, err := sql.Open("sqlite3", filepath.Join(dataDir, "komari.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var value string
	if err := db.QueryRow(`SELECT value FROM state`).Scan(&value); err != nil {
		t.Fatal(err)
	}
	if value != "new" {
		t.Fatalf("restored value = %q, want new", value)
	}
	archives, err := filepath.Glob(filepath.Join(root, "backup", "pre-restore-*.zip"))
	if err != nil || len(archives) != 1 {
		t.Fatalf("pre-restore archives = %#v, err=%v", archives, err)
	}
}

func TestRestoreStagedBackupKeepsCurrentDataWhenArchiveIsInvalid(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	current := filepath.Join(dataDir, "current.txt")
	if err := os.WriteFile(current, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "backup.zip"), []byte("not a zip"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := restoreStagedBackup(dataDir); err == nil {
		t.Fatal("invalid archive was restored")
	}
	content, err := os.ReadFile(current)
	if err != nil || string(content) != "keep" {
		t.Fatalf("current data changed: %q, err=%v", content, err)
	}
}

func TestAutomaticBackupRestoresPortableExportQueueAndFencesOldConnection(t *testing.T) {
	root := t.TempDir()
	data := filepath.Join(root, "data")
	if err := os.MkdirAll(data, 0700); err != nil {
		t.Fatal(err)
	}
	createRestoreSQLite(t, filepath.Join(data, "komari.db"), "original")
	q, err := historyexport.Open(filepath.Join(data, "exports-v2"))
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	j, err := q.Create(context.Background(), historyexport.Request{Owner: "owner", UUID: "node", Mode: "raw", Metrics: []string{"cpu"}, Start: time.Now().Add(-time.Hour), End: time.Now(), Source: historyexport.Source{DSN: filepath.Join(root, "old", "metrics.db"), Telemetry: metric.TelemetryConfig{ArchiveDirectory: filepath.Join(root, "old", "telemetry")}}})
	if err != nil {
		t.Fatal(err)
	}
	if err = backupDataDirectory(data, filepath.Join(data, "backup.zip"), map[string]struct{}{filepath.Join(data, "backup.zip"): {}}, false); err != nil {
		t.Fatal(err)
	}
	if err = restoreStagedBackup(data); err != nil {
		t.Fatal(err)
	}
	if _, err = q.List(context.Background(), "owner"); err != historyexport.ErrQueueReplaced {
		t.Fatal("old writer was not fenced", err)
	}
	fresh, err := historyexport.Open(filepath.Join(data, "exports-v2"))
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	restored, err := fresh.Get(context.Background(), "owner", j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Source.DSN != filepath.Join(data, "metrics.db") || restored.Status != "queued" {
		t.Fatal(restored)
	}
	archives, err := filepath.Glob(filepath.Join(root, "backup", "pre-restore-*.zip"))
	if err != nil || len(archives) != 1 {
		t.Fatal(archives, err)
	}
	z, err := zip.OpenReader(archives[0])
	if err != nil {
		t.Fatal(err)
	}
	defer z.Close()
	found := false
	for _, f := range z.File {
		if f.Name == "exports-v2/backup-manifest.json" {
			found = true
		}
	}
	if !found {
		t.Fatal("pre-restore safety backup omitted portable export manifest")
	}
}

func TestRestoreStagedBackupMakesReadOnlySQLiteWritableAndKeepsPrivateData0600(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	createRestoreSQLite(t, filepath.Join(dataDir, "komari.db"), "old")

	newDB := filepath.Join(root, "readonly-new.db")
	createRestoreSQLite(t, newDB, "from-readonly-zip")

	// Build a ZIP where komari.db is explicitly marked 0444 (read-only) and
	// plugin-data/komari-passkey/passkeys.json is marked 0644 (overly permissive).
	archivePath := filepath.Join(dataDir, "backup.zip")
	archive, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(archive)

	dbHeader := &zip.FileHeader{Name: "komari.db", Method: zip.Deflate}
	dbHeader.SetMode(0o444)
	dbEntry, err := zw.CreateHeader(dbHeader)
	if err != nil {
		t.Fatal(err)
	}
	srcDB, err := os.Open(newDB)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(dbEntry, srcDB); err != nil {
		srcDB.Close()
		t.Fatal(err)
	}
	srcDB.Close()

	pkHeader := &zip.FileHeader{Name: "plugin-data/komari-passkey/passkeys.json", Method: zip.Deflate}
	pkHeader.SetMode(0o644)
	pkEntry, err := zw.CreateHeader(pkHeader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pkEntry.Write([]byte(`{"version":1,"credentials":[]}`)); err != nil {
		t.Fatal(err)
	}

	marker, err := zw.Create("komari-backup-markup")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := marker.Write([]byte("backup")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}

	if err := RestoreStagedBackup(dataDir); err != nil {
		t.Fatalf("restore readonly archive: %v", err)
	}

	if runtime.GOOS != "windows" {
		dbInfo, err := os.Stat(filepath.Join(dataDir, "komari.db"))
		if err != nil {
			t.Fatal(err)
		}
		if dbInfo.Mode().Perm()&0o600 != 0o600 {
			t.Fatalf("restored komari.db perm = %04o, must be owner-writable (>=0600)", dbInfo.Mode().Perm())
		}
		pkInfo, err := os.Stat(filepath.Join(dataDir, "plugin-data", "komari-passkey", "passkeys.json"))
		if err != nil {
			t.Fatal(err)
		}
		if pkInfo.Mode().Perm() != 0o600 {
			t.Fatalf("restored passkeys.json perm = %04o, want 0600", pkInfo.Mode().Perm())
		}
	}

	// Prove SQLite can actually write to the restored komari.db.
	db, err := sql.Open("sqlite3", filepath.Join(dataDir, "komari.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO state VALUES ('post-restore-write')`); err != nil {
		t.Fatalf("write to restored komari.db failed: %v", err)
	}
}
