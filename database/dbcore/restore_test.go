package dbcore

import (
	"archive/zip"
	"context"
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/komari-monitor/komari/cmd/flags"
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

func TestRestoreStagedBackupPreservesPasskeyCountersAndRevocations(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}

	initPasskeyDB := func(path string) *sql.DB {
		db, err := sql.Open("sqlite3", path)
		if err != nil {
			t.Fatal(err)
		}
		stmts := []string{
			`CREATE TABLE users (uuid varchar(36) PRIMARY KEY, username text)`,
			`CREATE TABLE user_passkeys (credential_hash char(64) PRIMARY KEY, credential_id text NOT NULL, user_uuid varchar(36) NOT NULL, rp_id varchar(253) NOT NULL, name varchar(80) NOT NULL, public_key text NOT NULL, device_type text, backed_up integer)`,
			`CREATE TABLE passkey_counters (user_uuid varchar(36), key_digest varchar(64), counter integer, PRIMARY KEY (user_uuid, key_digest))`,
			`CREATE TABLE passkey_revocations (credential_hash char(64) PRIMARY KEY, user_uuid varchar(36) NOT NULL, rp_id varchar(253) NOT NULL, revoked_at datetime NOT NULL)`,
			`INSERT INTO users VALUES ('u1', 'admin')`,
			`INSERT INTO user_passkeys VALUES ('hash-a', 'cred-a', 'u1', 'panel.example.test', 'Key A', 'pub-a', 'singleDevice', 0)`,
			`INSERT INTO user_passkeys VALUES ('hash-b', 'cred-b', 'u1', 'panel.example.test', 'Key B', 'pub-b', 'singleDevice', 0)`,
			`INSERT INTO passkey_counters VALUES ('u1', 'digest-a', 2)`,
			`INSERT INTO passkey_counters VALUES ('u1', 'digest-b', 1)`,
		}
		for _, s := range stmts {
			if _, err := db.Exec(s); err != nil {
				db.Close()
				t.Fatalf("exec %q: %v", s, err)
			}
		}
		return db
	}

	// Backup snapshot contains Key A (counter=2) and Key B (counter=1).
	backupDBPath := filepath.Join(root, "backup-src.db")
	bdb := initPasskeyDB(backupDBPath)
	if err := bdb.Close(); err != nil {
		t.Fatal(err)
	}
	writeRestoreArchive(t, filepath.Join(dataDir, "backup.zip"), backupDBPath)

	// Live DB has advanced Key A counter to 25 and explicitly revoked Key B.
	liveDBPath := filepath.Join(dataDir, "komari.db")
	ldb := initPasskeyDB(liveDBPath)
	if _, err := ldb.Exec(`UPDATE passkey_counters SET counter = 25 WHERE user_uuid = 'u1' AND key_digest = 'digest-a'`); err != nil {
		t.Fatal(err)
	}
	if _, err := ldb.Exec(`DELETE FROM user_passkeys WHERE credential_hash = 'hash-b'`); err != nil {
		t.Fatal(err)
	}
	if _, err := ldb.Exec(`INSERT INTO passkey_revocations VALUES ('hash-b', 'u1', 'panel.example.test', '2026-11-20T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if err := ldb.Close(); err != nil {
		t.Fatal(err)
	}

	if err := restoreStagedBackup(dataDir); err != nil {
		t.Fatalf("restoreStagedBackup failed: %v", err)
	}

	restored, err := sql.Open("sqlite3", filepath.Join(dataDir, "komari.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()

	var counterA uint32
	if err := restored.QueryRow(`SELECT counter FROM passkey_counters WHERE user_uuid = 'u1' AND key_digest = 'digest-a'`).Scan(&counterA); err != nil {
		t.Fatal(err)
	}
	if counterA != 25 {
		t.Fatalf("Key A counter rolled back to %d, want 25", counterA)
	}

	var keyBCount int
	if err := restored.QueryRow(`SELECT COUNT(*) FROM user_passkeys WHERE credential_hash = 'hash-b'`).Scan(&keyBCount); err != nil {
		t.Fatal(err)
	}
	if keyBCount != 0 {
		t.Fatalf("revoked Key B was resurrected in user_passkeys (count=%d)", keyBCount)
	}

	var revCount int
	if err := restored.QueryRow(`SELECT COUNT(*) FROM passkey_revocations WHERE credential_hash = 'hash-b'`).Scan(&revCount); err != nil {
		t.Fatal(err)
	}
	if revCount != 1 {
		t.Fatalf("revocation tombstone for Key B was lost (count=%d)", revCount)
	}
	restored.Close()

	// Also verify custom --database (flags.DatabaseFile) outside dataDir/komari.db
	// is consulted when preserving high-water counters and revocations across restore.
	customRoot := t.TempDir()
	customDataDir := filepath.Join(customRoot, "data")
	if err := os.MkdirAll(customDataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeRestoreArchive(t, filepath.Join(customDataDir, "backup.zip"), backupDBPath)

	customLivePath := filepath.Join(customRoot, "custom-primary.db")
	customLiveDB := initPasskeyDB(customLivePath)
	if _, err := customLiveDB.Exec(`UPDATE passkey_counters SET counter = 42 WHERE user_uuid = 'u1' AND key_digest = 'digest-a'`); err != nil {
		t.Fatal(err)
	}
	if _, err := customLiveDB.Exec(`DELETE FROM user_passkeys WHERE credential_hash = 'hash-b'`); err != nil {
		t.Fatal(err)
	}
	if _, err := customLiveDB.Exec(`INSERT INTO passkey_revocations VALUES ('hash-b', 'u1', 'panel.example.test', '2026-11-20T01:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if err := customLiveDB.Close(); err != nil {
		t.Fatal(err)
	}

	origDBFile := flags.DatabaseFile
	flags.DatabaseFile = customLivePath
	t.Cleanup(func() { flags.DatabaseFile = origDBFile })

	if err := restoreStagedBackup(customDataDir); err != nil {
		t.Fatalf("restoreStagedBackup with custom --database failed: %v", err)
	}
	// Open along the actual startup DSN path (buildSQLiteDSN(flags.DatabaseFile))
	// rather than data/komari.db to prove the service opens the restored+merged DB.
	customRestored, err := sql.Open("sqlite3", buildSQLiteDSN(flags.DatabaseFile))
	if err != nil {
		t.Fatal(err)
	}
	defer customRestored.Close()
	if err := customRestored.QueryRow(`SELECT counter FROM passkey_counters WHERE user_uuid = 'u1' AND key_digest = 'digest-a'`).Scan(&counterA); err != nil || counterA != 42 {
		t.Fatalf("custom --database counterA = %d (err=%v), want 42", counterA, err)
	}
	if err := customRestored.QueryRow(`SELECT COUNT(*) FROM user_passkeys WHERE credential_hash = 'hash-b'`).Scan(&keyBCount); err != nil || keyBCount != 0 {
		t.Fatalf("custom --database revoked Key B resurrected (count=%d err=%v)", keyBCount, err)
	}
	customRestored.Close()

	// Also verify custom --database inside data/ (e.g. data/custom.db) when the
	// backup archive only contains canonical komari.db: startup must open the
	// restored database at data/custom.db rather than creating a blank SQLite file.
	innerRoot := t.TempDir()
	innerDataDir := filepath.Join(innerRoot, "data")
	if err := os.MkdirAll(innerDataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	innerCustomPath := filepath.Join(innerDataDir, "custom.db")
	innerLiveDB := initPasskeyDB(innerCustomPath)
	if _, err := innerLiveDB.Exec(`UPDATE passkey_counters SET counter = 77 WHERE user_uuid = 'u1' AND key_digest = 'digest-a'`); err != nil {
		t.Fatal(err)
	}
	if _, err := innerLiveDB.Exec(`DELETE FROM user_passkeys WHERE credential_hash = 'hash-b'`); err != nil {
		t.Fatal(err)
	}
	if _, err := innerLiveDB.Exec(`INSERT INTO passkey_revocations VALUES ('hash-b', 'u1', 'panel.example.test', '2026-11-20T02:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if err := innerLiveDB.Close(); err != nil {
		t.Fatal(err)
	}
	writeRestoreArchive(t, filepath.Join(innerDataDir, "backup.zip"), backupDBPath)

	flags.DatabaseFile = innerCustomPath
	if err := restoreStagedBackup(innerDataDir); err != nil {
		t.Fatalf("restoreStagedBackup with data/custom.db failed: %v", err)
	}
	innerRestored, err := sql.Open("sqlite3", buildSQLiteDSN(flags.DatabaseFile))
	if err != nil {
		t.Fatal(err)
	}
	defer innerRestored.Close()
	if err := innerRestored.QueryRow(`SELECT counter FROM passkey_counters WHERE user_uuid = 'u1' AND key_digest = 'digest-a'`).Scan(&counterA); err != nil || counterA != 77 {
		t.Fatalf("data/custom.db counterA = %d (err=%v), want 77", counterA, err)
	}
	if err := innerRestored.QueryRow(`SELECT COUNT(*) FROM user_passkeys WHERE credential_hash = 'hash-b'`).Scan(&keyBCount); err != nil || keyBCount != 0 {
		t.Fatalf("data/custom.db revoked Key B resurrected (count=%d err=%v)", keyBCount, err)
	}
}

func TestRestoreStagedBackupRetiresCaseVariantPasskeyAndSnapshotsExternalDB(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	extDir := filepath.Join(root, "external")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(extDir, 0o755); err != nil {
		t.Fatal(err)
	}
	extDBPath := filepath.Join(extDir, "custom.db")
	extDB, err := sql.Open("sqlite3", extDBPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := extDB.Exec(`CREATE TABLE marker (v text); INSERT INTO marker VALUES ('live-ext');`); err != nil {
		t.Fatal(err)
	}
	if err := extDB.Close(); err != nil {
		t.Fatal(err)
	}
	backupDBPath := filepath.Join(root, "backup-src.db")
	backupDB, err := sql.Open("sqlite3", backupDBPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backupDB.Exec(`CREATE TABLE marker (v text); INSERT INTO marker VALUES ('restored-backup');`); err != nil {
		t.Fatal(err)
	}
	if err := backupDB.Close(); err != nil {
		t.Fatal(err)
	}

	archivePath := filepath.Join(dataDir, "backup.zip")
	f, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	addBytes := func(name string, payload []byte) {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(payload); err != nil {
			t.Fatal(err)
		}
	}
	rawDB, err := os.ReadFile(backupDBPath)
	if err != nil {
		t.Fatal(err)
	}
	addBytes("komari.db", rawDB)
	addBytes("plugin/Komari-Passkey/komari-plugin.json", []byte(`{"short":"komari-passkey","name":"Legacy Passkey"}`))
	addBytes("plugin/Komari-Passkey/.komari-package.json", []byte(`{"digest":"abc"}`))
	addBytes("plugin/.packages/deadbeef.json", []byte(`{"short":"Komari-Passkey","digest":"deadbeef"}`))
	addBytes("plugin/.packages/deadbeef.zip", []byte("zipdata"))
	addBytes("plugin/state.json", []byte(`{"plugins":{"Komari-Passkey":{"enabled":true,"approved_permissions_hash":"h1","approved_package_digest":"deadbeef"}}}`))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	prevDBFile := flags.DatabaseFile
	flags.DatabaseFile = extDBPath
	defer func() { flags.DatabaseFile = prevDBFile }()

	if err := restoreStagedBackup(dataDir); err != nil {
		t.Fatalf("restoreStagedBackup failed: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dataDir, "plugin", "Komari-Passkey", ".komari-package.json")); !os.IsNotExist(err) {
		t.Fatalf("legacy passkey receipt was not removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "plugin", ".packages", "deadbeef.json")); !os.IsNotExist(err) {
		t.Fatalf("staged legacy passkey package metadata was not removed: %v", err)
	}
	stateRaw, err := os.ReadFile(filepath.Join(dataDir, "plugin", "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stateRaw), `"enabled": true`) {
		t.Fatalf("case-variant Komari-Passkey remained enabled in state.json: %s", stateRaw)
	}

	preZips, err := filepath.Glob(filepath.Join(root, "backup", "pre-restore-*.zip"))
	if err != nil || len(preZips) != 1 {
		t.Fatalf("expected 1 pre-restore zip, got %v (err=%v)", preZips, err)
	}
	zr, err := zip.OpenReader(preZips[0])
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	foundExt := false
	for _, entry := range zr.File {
		if entry.Name == "external-database/custom.db" {
			foundExt = true
			break
		}
	}
	if !foundExt {
		t.Fatalf("external --database file was not archived in pre-restore zip")
	}
}
