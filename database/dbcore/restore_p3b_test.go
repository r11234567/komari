package dbcore

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestRestoreStagedBackupHandlesReadOnlyMountParentSameVolume(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("read-only parent mount simulation requires POSIX non-root permissions")
	}
	t.Cleanup(func() { SetCurrentSecurityEpoch(1) })

	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	createRestoreSQLite(t, filepath.Join(dataDir, "komari.db"), "old-mount")
	newDB := filepath.Join(t.TempDir(), "new.db")
	createRestoreSQLite(t, newDB, "new-mount")
	writeRestoreArchive(t, filepath.Join(dataDir, "backup.zip"), newDB)

	if err := os.Chmod(root, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o755) })

	if err := restoreStagedBackup(dataDir); err != nil {
		t.Fatalf("restoreStagedBackup with read-only mount parent failed: %v", err)
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
	if value != "new-mount" {
		t.Fatalf("restored value = %q, want new-mount", value)
	}
	preZips, err := filepath.Glob(filepath.Join(dataDir, fallbackPreRestoreDir, "pre-restore-*.zip"))
	if err != nil || len(preZips) != 1 {
		t.Fatalf("expected fallback pre-restore zip under dataDir, got %v (err=%v)", preZips, err)
	}
}

func TestRecoverPendingGenerationCompletesInterruptedSwitch(t *testing.T) {
	for _, stage := range []string{"park", "publish"} {
		t.Run("crash_after_"+stage, func(t *testing.T) {
			t.Cleanup(func() {
				SetCurrentSecurityEpoch(1)
				restoreFaultHook.afterManifestPark = nil
				restoreFaultHook.afterManifestPublish = nil
			})

			root := t.TempDir()
			dataDir := filepath.Join(root, "data")
			if err := os.MkdirAll(dataDir, 0o755); err != nil {
				t.Fatal(err)
			}
			createRestoreSQLite(t, filepath.Join(dataDir, "komari.db"), "old")
			if err := os.WriteFile(filepath.Join(dataDir, "obsolete.txt"), []byte("remove-me"), 0o600); err != nil {
				t.Fatal(err)
			}
			orphanDir := filepath.Join(dataDir, generationsDirName, "gen-orphan-1")
			if err := os.MkdirAll(orphanDir, 0o700); err != nil {
				t.Fatal(err)
			}

			newDB := filepath.Join(root, "new.db")
			createRestoreSQLite(t, newDB, "recovered-new")
			writeRestoreArchive(t, filepath.Join(dataDir, "backup.zip"), newDB)

			boom := errors.New("simulated crash")
			if stage == "park" {
				restoreFaultHook.afterManifestPark = func() error { return boom }
			} else {
				restoreFaultHook.afterManifestPublish = func() error { return boom }
			}

			err := restoreStagedBackup(dataDir)
			if !errors.Is(err, boom) {
				t.Fatalf("expected simulated crash error, got %v", err)
			}
			restoreFaultHook.afterManifestPark = nil
			restoreFaultHook.afterManifestPublish = nil

			// Simulate process reboot completing pending switch before opening DB.
			if err := RecoverPendingGeneration(dataDir); err != nil {
				t.Fatalf("RecoverPendingGeneration failed: %v", err)
			}
			if _, err := os.Stat(filepath.Join(dataDir, "obsolete.txt")); !os.IsNotExist(err) {
				t.Fatalf("obsolete entry survived recovered switch: %v", err)
			}
			if _, err := os.Stat(filepath.Join(dataDir, "backup.zip")); !os.IsNotExist(err) {
				t.Fatalf("backup.zip survived recovered switch: %v", err)
			}
			if entries, err := os.ReadDir(filepath.Join(dataDir, generationsDirName)); err == nil && len(entries) != 0 {
				t.Fatalf("leftover generation dirs after recovery: %#v", entries)
			}
			epoch, err := readSecurityEpochFile(dataDir)
			if err != nil || epoch < 2 {
				t.Fatalf("recovered epoch = %d (err=%v), want >= 2", epoch, err)
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
			if value != "recovered-new" {
				t.Fatalf("recovered value = %q, want recovered-new", value)
			}
		})
	}
}

func TestRestoreReconcilesSecurityEpochPurgesSessionsAndCarriesRevocations(t *testing.T) {
	t.Cleanup(func() { SetCurrentSecurityEpoch(1) })

	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}

	initSchema := func(dbPath string, revokedNow bool, withSession bool) {
		db, err := sql.Open("sqlite3", dbPath)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		stmts := []string{
			`CREATE TABLE users (uuid varchar(36) PRIMARY KEY)`,
			`CREATE TABLE sessions (uuid varchar(36), session varchar(255) PRIMARY KEY, security_epoch integer DEFAULT 1)`,
			`CREATE TABLE install_tokens (token varchar(128) PRIMARY KEY, client varchar(36))`,
			`CREATE TABLE agent_enrollments (device_code varchar(128) PRIMARY KEY)`,
			`CREATE TABLE api_tokens (id varchar(36) PRIMARY KEY, revoked_at datetime)`,
			`CREATE TABLE two_factor_counters (uuid varchar(36), counter integer, used_at datetime, expires_at datetime, PRIMARY KEY (uuid, counter))`,
			`CREATE TABLE agent_credentials (client varchar(36) PRIMARY KEY, agent_public_key text, revoked_at datetime, revoked_reason varchar(255), refresh_recovery_sealed text, refresh_recovery_request varchar(64), refresh_recovery_until datetime, security_epoch integer DEFAULT 1)`,
			`CREATE TABLE agent_refresh_uses (token_hash varchar(64) PRIMARY KEY, client varchar(36), request_hash varchar(64), generation integer, consumed_at datetime, expires_at datetime)`,
			`CREATE TABLE security_epoch_states (id integer PRIMARY KEY, epoch integer NOT NULL DEFAULT 1, updated_at datetime)`,
			`INSERT INTO users (uuid) VALUES ('user-1')`,
			`INSERT INTO api_tokens (id, revoked_at) VALUES ('pat-1', NULL)`,
			`INSERT INTO agent_credentials (client, agent_public_key, revoked_at, security_epoch) VALUES ('agent-1', 'pubkey-1', NULL, 1)`,
		}
		for _, stmt := range stmts {
			if _, err := db.Exec(stmt); err != nil {
				t.Fatal(err)
			}
		}
		if withSession {
			_, _ = db.Exec(`INSERT INTO sessions (uuid, session, security_epoch) VALUES ('user-1', 'v1:deadbeef', 1)`)
			_, _ = db.Exec(`INSERT INTO install_tokens (token, client) VALUES ('ki_old', 'agent-1')`)
			_, _ = db.Exec(`INSERT INTO agent_enrollments (device_code) VALUES ('dev-old')`)
		}
		if revokedNow {
			now := time.Now().UTC().Format(time.RFC3339Nano)
			_, _ = db.Exec(`UPDATE api_tokens SET revoked_at = ? WHERE id = 'pat-1'`, now)
			_, _ = db.Exec(`UPDATE agent_credentials SET revoked_at = ?, revoked_reason = 'compromised' WHERE client = 'agent-1'`, now)
			_, _ = db.Exec(`INSERT INTO two_factor_counters (uuid, counter, used_at, expires_at) VALUES ('user-1', 42, ?, ?)`, now, now)
			_, _ = db.Exec(`INSERT INTO agent_refresh_uses (token_hash, client, request_hash, generation, consumed_at, expires_at) VALUES ('spent-rt-hash', 'agent-1', 'req-1', 2, ?, ?)`, now, now)
			_, _ = db.Exec(`INSERT INTO security_epoch_states (id, epoch, updated_at) VALUES (1, 3, ?)`, now)
		}
	}

	initSchema(filepath.Join(dataDir, "komari.db"), true, false)
	snapshotDB := filepath.Join(root, "older-backup.db")
	initSchema(snapshotDB, false, true)
	writeRestoreArchive(t, filepath.Join(dataDir, "backup.zip"), snapshotDB)

	if err := restoreStagedBackup(dataDir); err != nil {
		t.Fatalf("restoreStagedBackup failed: %v", err)
	}
	epoch, err := readSecurityEpochFile(dataDir)
	if err != nil || epoch != 4 {
		t.Fatalf("restored epoch = %d (err=%v), want 4 (live epoch 3 + 1)", epoch, err)
	}

	db, err := sql.Open("sqlite3", filepath.Join(dataDir, "komari.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var count int
	for _, tbl := range []string{"sessions", "install_tokens", "agent_enrollments"} {
		if err := db.QueryRow(`SELECT COUNT(*) FROM ` + tbl).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("table %s still has %d rows after restore", tbl, count)
		}
	}
	var dbEpoch uint64
	if err := db.QueryRow(`SELECT epoch FROM security_epoch_states WHERE id = 1`).Scan(&dbEpoch); err != nil || dbEpoch != 4 {
		t.Fatalf("db security epoch = %d (err=%v), want 4", dbEpoch, err)
	}
	var patRevoked sql.NullString
	if err := db.QueryRow(`SELECT revoked_at FROM api_tokens WHERE id = 'pat-1'`).Scan(&patRevoked); err != nil || !patRevoked.Valid {
		t.Fatalf("live PAT revocation did not carry forward: valid=%v err=%v", patRevoked.Valid, err)
	}
	var agentRevoked sql.NullString
	var agentReason string
	if err := db.QueryRow(`SELECT revoked_at, revoked_reason FROM agent_credentials WHERE client = 'agent-1'`).Scan(&agentRevoked, &agentReason); err != nil || !agentRevoked.Valid || agentReason != "compromised" {
		t.Fatalf("live agent revocation did not carry forward: valid=%v reason=%q err=%v", agentRevoked.Valid, agentReason, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM two_factor_counters WHERE uuid = 'user-1' AND counter = 42`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("two_factor_counters not preserved: count=%d err=%v", count, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM agent_refresh_uses WHERE token_hash = 'spent-rt-hash'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("agent_refresh_uses not carried forward: count=%d err=%v", count, err)
	}
}
