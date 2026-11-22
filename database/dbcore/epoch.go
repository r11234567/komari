package dbcore

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/komari-monitor/komari/database/models"
	"gorm.io/gorm"
)

const securityEpochFileName = "security-epoch.json"

var activeSecurityEpoch atomic.Uint64

// CurrentSecurityEpoch returns the active control-plane security epoch (>= 1).
func CurrentSecurityEpoch() uint64 {
	if epoch := activeSecurityEpoch.Load(); epoch > 0 {
		return epoch
	}
	return 1
}

// SetCurrentSecurityEpoch updates the process-wide active security epoch.
func SetCurrentSecurityEpoch(epoch uint64) {
	if epoch == 0 {
		epoch = 1
	}
	activeSecurityEpoch.Store(epoch)
}

type securityEpochFileDoc struct {
	Version       int       `json:"version"`
	SecurityEpoch uint64    `json:"security_epoch"`
	UpdatedAt     time.Time `json:"updated_at"`
}

func readSecurityEpochFile(dir string) (uint64, error) {
	if strings.TrimSpace(dir) == "" {
		return 0, nil
	}
	raw, err := os.ReadFile(filepath.Join(dir, securityEpochFileName))
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	var doc securityEpochFileDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return 0, fmt.Errorf("parse %s: %w", securityEpochFileName, err)
	}
	return doc.SecurityEpoch, nil
}

func writeSecurityEpochFile(dir string, epoch uint64) error {
	if epoch == 0 {
		epoch = 1
	}
	return atomicWriteJSON(filepath.Join(dir, securityEpochFileName), securityEpochFileDoc{
		Version:       1,
		SecurityEpoch: epoch,
		UpdatedAt:     time.Now().UTC(),
	}, 0o600)
}

func epochMarkerDirForStartup(defaultDataDir string) string {
	if custom := configuredSQLiteFilePath(); isCustomConfiguredSQLitePath(defaultDataDir, custom) {
		return filepath.Dir(custom)
	}
	if _, err := os.Stat(defaultDataDir); err == nil {
		return defaultDataDir
	}
	return ""
}

func reconcileStartupSecurityEpoch(defaultDataDir string, db *gorm.DB) (uint64, error) {
	markerDir := epochMarkerDirForStartup(defaultDataDir)
	fileEpoch, err := readSecurityEpochFile(markerDir)
	if err != nil {
		return 0, err
	}
	var dbEpoch uint64
	if db.Migrator().HasTable(&models.SecurityEpochState{}) {
		var row models.SecurityEpochState
		if err := db.Limit(1).Order("id ASC").Find(&row).Error; err == nil && row.Epoch > dbEpoch {
			dbEpoch = row.Epoch
		}
	}
	epoch := max(max(fileEpoch, dbEpoch), 1)
	now := time.Now().UTC()
	if db.Migrator().HasTable(&models.SecurityEpochState{}) {
		if err := db.Exec(
			`INSERT INTO security_epoch_states (id, epoch, updated_at) VALUES (1, ?, ?)
			 ON CONFLICT(id) DO UPDATE SET epoch = CASE WHEN security_epoch_states.epoch > excluded.epoch THEN security_epoch_states.epoch ELSE excluded.epoch END, updated_at = excluded.updated_at`,
			epoch, now,
		).Error; err != nil {
			return 0, fmt.Errorf("persist database security epoch: %w", err)
		}
	}
	if epoch == 1 {
		if db.Migrator().HasTable(&models.Session{}) {
			if err := db.Model(&models.Session{}).Where("security_epoch = 0 OR security_epoch IS NULL").Update("security_epoch", uint64(1)).Error; err != nil {
				return 0, fmt.Errorf("backfill session security epoch: %w", err)
			}
		}
		if db.Migrator().HasTable(&models.AgentCredential{}) {
			if err := db.Model(&models.AgentCredential{}).Where("security_epoch = 0 OR security_epoch IS NULL").Update("security_epoch", uint64(1)).Error; err != nil {
				return 0, fmt.Errorf("backfill agent credential security epoch: %w", err)
			}
		}
	} else {
		if db.Migrator().HasTable(&models.Session{}) {
			if err := db.Where("security_epoch < ?", epoch).Delete(&models.Session{}).Error; err != nil {
				return 0, fmt.Errorf("purge pre-epoch sessions: %w", err)
			}
		}
		if db.Migrator().HasTable(&models.InstallToken{}) {
			if err := db.Where("used_at IS NOT NULL OR expires_at <= ?", now).Delete(&models.InstallToken{}).Error; err != nil {
				return 0, fmt.Errorf("clean expired install tokens: %w", err)
			}
		}
	}
	if markerDir != "" {
		if err := writeSecurityEpochFile(markerDir, epoch); err != nil {
			return 0, fmt.Errorf("write startup security epoch marker: %w", err)
		}
	}
	SetCurrentSecurityEpoch(epoch)
	return epoch, nil
}

type restoreSecurityCarryover struct {
	passkeyCounters      map[counterCarryKey]uint32
	passkeyRevocations   map[string]passkeyRevocationRow
	revokedAPITokens     map[string]string
	twoFactorCounters    []twoFactorCounterRow
	agentRevocations     map[string]agentRevocationRow
	agentRefreshUses     map[string]agentRefreshUseRow
	maxLiveSecurityEpoch uint64
}

type counterCarryKey struct {
	userUUID  string
	keyDigest string
}

type passkeyRevocationRow struct {
	credentialHash string
	userUUID       string
	rpID           string
	revokedAt      string
}

type twoFactorCounterRow struct {
	uuid      string
	counter   int64
	usedAt    string
	expiresAt string
}

type agentRevocationRow struct {
	client        string
	revokedAt     string
	revokedReason string
	publicKey     string
}

type agentRefreshUseRow struct {
	tokenHash   string
	client      string
	requestHash string
	generation  uint64
	consumedAt  string
	expiresAt   string
}

func readSQLiteSecurityEpoch(db *sql.DB) (uint64, error) {
	hasTable, err := sqliteTableExists(db, "security_epoch_states")
	if err != nil || !hasTable {
		return 0, err
	}
	var epoch uint64
	if err := db.QueryRow(`SELECT COALESCE(MAX(epoch), 0) FROM security_epoch_states`).Scan(&epoch); err != nil {
		return 0, err
	}
	return epoch, nil
}

func collectLiveSecurityState(dataDir string) (restoreSecurityCarryover, uint64, error) {
	sources := []string{filepath.Join(dataDir, "komari.db")}
	if custom := configuredSQLiteFilePath(); isCustomConfiguredSQLitePath(dataDir, custom) {
		sources = append(sources, custom)
		if base := filepath.Base(custom); base != "" && base != "komari.db" {
			sources = append(sources, filepath.Join(dataDir, base))
		}
	}

	carry := restoreSecurityCarryover{
		passkeyCounters:    make(map[counterCarryKey]uint32),
		passkeyRevocations: make(map[string]passkeyRevocationRow),
		revokedAPITokens:   make(map[string]string),
		agentRevocations:   make(map[string]agentRevocationRow),
		agentRefreshUses:   make(map[string]agentRefreshUseRow),
	}
	fileEpoch, err := readSecurityEpochFile(dataDir)
	if err != nil {
		return carry, 0, err
	}
	prevEpoch := fileEpoch
	seenPaths := make(map[string]bool, len(sources))
	tfSeen := make(map[string]bool)

	for _, currentPath := range sources {
		if strings.TrimSpace(currentPath) == "" {
			continue
		}
		absPath, err := filepath.Abs(currentPath)
		if err != nil {
			absPath = filepath.Clean(currentPath)
		}
		if seenPaths[absPath] {
			continue
		}
		seenPaths[absPath] = true
		if _, err := os.Stat(currentPath); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return carry, 0, err
		}
		if err := validateRestoredSQLite(currentPath, "current komari.db"); err != nil {
			continue
		}

		curDB, err := sql.Open("sqlite3", currentPath)
		if err != nil {
			return carry, 0, err
		}
		if err := readCarryoverFromDB(curDB, &carry, tfSeen); err != nil {
			_ = curDB.Close()
			return carry, 0, err
		}
		_ = curDB.Close()
	}
	if carry.maxLiveSecurityEpoch > prevEpoch {
		prevEpoch = carry.maxLiveSecurityEpoch
	}
	return carry, prevEpoch, nil
}

func readCarryoverFromDB(curDB *sql.DB, carry *restoreSecurityCarryover, tfSeen map[string]bool) error {
	if epoch, err := readSQLiteSecurityEpoch(curDB); err != nil {
		return err
	} else if epoch > carry.maxLiveSecurityEpoch {
		carry.maxLiveSecurityEpoch = epoch
	}

	if has, err := sqliteTableExists(curDB, "passkey_counters"); err != nil {
		return err
	} else if has {
		rows, err := curDB.Query(`SELECT user_uuid, key_digest, counter FROM passkey_counters`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var userUUID, keyDigest string
			var counter uint32
			if err := rows.Scan(&userUUID, &keyDigest, &counter); err != nil {
				_ = rows.Close()
				return err
			}
			k := counterCarryKey{userUUID: userUUID, keyDigest: keyDigest}
			if counter > carry.passkeyCounters[k] {
				carry.passkeyCounters[k] = counter
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		_ = rows.Close()
	}

	if has, err := sqliteTableExists(curDB, "passkey_revocations"); err != nil {
		return err
	} else if has {
		rows, err := curDB.Query(`SELECT credential_hash, user_uuid, rp_id, COALESCE(CAST(revoked_at AS TEXT), '') FROM passkey_revocations`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var r passkeyRevocationRow
			if err := rows.Scan(&r.credentialHash, &r.userUUID, &r.rpID, &r.revokedAt); err != nil {
				_ = rows.Close()
				return err
			}
			if r.revokedAt == "" {
				r.revokedAt = time.Now().UTC().Format(time.RFC3339Nano)
			}
			carry.passkeyRevocations[r.credentialHash] = r
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		_ = rows.Close()
	}

	if has, err := sqliteTableExists(curDB, "api_tokens"); err != nil {
		return err
	} else if has {
		rows, err := curDB.Query(`SELECT id, CAST(revoked_at AS TEXT) FROM api_tokens WHERE revoked_at IS NOT NULL`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id, revokedAt string
			if err := rows.Scan(&id, &revokedAt); err != nil {
				_ = rows.Close()
				return err
			}
			if revokedAt == "" {
				revokedAt = time.Now().UTC().Format(time.RFC3339Nano)
			}
			carry.revokedAPITokens[id] = revokedAt
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		_ = rows.Close()
	}

	if has, err := sqliteTableExists(curDB, "two_factor_counters"); err != nil {
		return err
	} else if has {
		rows, err := curDB.Query(`SELECT uuid, counter, COALESCE(CAST(used_at AS TEXT), ''), COALESCE(CAST(expires_at AS TEXT), '') FROM two_factor_counters`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var r twoFactorCounterRow
			if err := rows.Scan(&r.uuid, &r.counter, &r.usedAt, &r.expiresAt); err != nil {
				_ = rows.Close()
				return err
			}
			key := fmt.Sprintf("%s\x00%d", r.uuid, r.counter)
			if !tfSeen[key] {
				tfSeen[key] = true
				carry.twoFactorCounters = append(carry.twoFactorCounters, r)
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		_ = rows.Close()
	}

	if has, err := sqliteTableExists(curDB, "agent_credentials"); err != nil {
		return err
	} else if has {
		rows, err := curDB.Query(`SELECT client, COALESCE(CAST(revoked_at AS TEXT), ''), COALESCE(revoked_reason, ''), COALESCE(agent_public_key, '') FROM agent_credentials WHERE revoked_at IS NOT NULL`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var r agentRevocationRow
			if err := rows.Scan(&r.client, &r.revokedAt, &r.revokedReason, &r.publicKey); err != nil {
				_ = rows.Close()
				return err
			}
			if r.revokedAt == "" {
				r.revokedAt = time.Now().UTC().Format(time.RFC3339Nano)
			}
			carry.agentRevocations[r.client] = r
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		_ = rows.Close()
	}

	if has, err := sqliteTableExists(curDB, "agent_refresh_uses"); err != nil {
		return err
	} else if has {
		rows, err := curDB.Query(`SELECT token_hash, client, COALESCE(request_hash, ''), COALESCE(generation, 0), COALESCE(CAST(consumed_at AS TEXT), ''), COALESCE(CAST(expires_at AS TEXT), '') FROM agent_refresh_uses`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var r agentRefreshUseRow
			if err := rows.Scan(&r.tokenHash, &r.client, &r.requestHash, &r.generation, &r.consumedAt, &r.expiresAt); err != nil {
				_ = rows.Close()
				return err
			}
			carry.agentRefreshUses[r.tokenHash] = r
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		_ = rows.Close()
	}
	return nil
}

func reconcileRestoreSecurityState(dataDir, stageDir string) (prevEpoch uint64, newEpoch uint64, err error) {
	carry, prevEpoch, err := collectLiveSecurityState(dataDir)
	if err != nil {
		return 0, 0, err
	}
	stagedKomari := filepath.Join(stageDir, "komari.db")
	newEpoch, err = applyRestoreSecurityStateToDB(stagedKomari, prevEpoch, carry)
	if err != nil {
		return 0, 0, err
	}
	if custom := configuredSQLiteFilePath(); isCustomConfiguredSQLitePath(dataDir, custom) {
		if base := filepath.Base(custom); base != "" && base != "komari.db" {
			stagedCustom := filepath.Join(stageDir, base)
			if _, statErr := os.Stat(stagedCustom); statErr == nil {
				customEpoch, customErr := applyRestoreSecurityStateToDB(stagedCustom, prevEpoch, carry)
				if customErr != nil {
					return 0, 0, customErr
				}
				if customEpoch > newEpoch {
					newEpoch = customEpoch
				}
			}
		}
	}
	if prevEpoch == 0 {
		prevEpoch = 1
	}
	return prevEpoch, newEpoch, nil
}

func applyRestoreSecurityStateToDB(stagedPath string, prevMaxEpoch uint64, carry restoreSecurityCarryover) (uint64, error) {
	stagedDB, err := sql.Open("sqlite3", stagedPath)
	if err != nil {
		return 0, err
	}
	stagedClosed := false
	defer func() {
		if !stagedClosed {
			_ = stagedDB.Close()
		}
	}()

	stagedEpoch, err := readSQLiteSecurityEpoch(stagedDB)
	if err != nil {
		return 0, err
	}
	newEpoch := max(max(prevMaxEpoch, stagedEpoch), 1) + 1

	stagedHasUsers, err := sqliteTableExists(stagedDB, "users")
	if err != nil {
		return 0, err
	}
	stagedHasPasskeys, err := sqliteTableExists(stagedDB, "user_passkeys")
	if err != nil {
		return 0, err
	}
	stagedHasCounters, err := sqliteTableExists(stagedDB, "passkey_counters")
	if err != nil {
		return 0, err
	}
	stagedHasRevocations, err := sqliteTableExists(stagedDB, "passkey_revocations")
	if err != nil {
		return 0, err
	}
	stagedHasSessions, err := sqliteTableExists(stagedDB, "sessions")
	if err != nil {
		return 0, err
	}
	stagedHasInstallTokens, err := sqliteTableExists(stagedDB, "install_tokens")
	if err != nil {
		return 0, err
	}
	stagedHasEnrollments, err := sqliteTableExists(stagedDB, "agent_enrollments")
	if err != nil {
		return 0, err
	}
	stagedHasAPITokens, err := sqliteTableExists(stagedDB, "api_tokens")
	if err != nil {
		return 0, err
	}
	stagedHasTwoFactorCounters, err := sqliteTableExists(stagedDB, "two_factor_counters")
	if err != nil {
		return 0, err
	}
	stagedHasAgentCreds, err := sqliteTableExists(stagedDB, "agent_credentials")
	if err != nil {
		return 0, err
	}
	stagedHasRefreshUses, err := sqliteTableExists(stagedDB, "agent_refresh_uses")
	if err != nil {
		return 0, err
	}

	tx, err := stagedDB.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS security_epoch_states (
		id integer PRIMARY KEY,
		epoch integer NOT NULL DEFAULT 1,
		updated_at datetime
	)`); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`INSERT INTO security_epoch_states (id, epoch, updated_at) VALUES (1, ?, ?)
		ON CONFLICT(id) DO UPDATE SET epoch = excluded.epoch, updated_at = excluded.updated_at`,
		newEpoch, time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		return 0, err
	}

	// Purge transient sessions, single-use install tokens, and pending device enrollments.
	if stagedHasSessions {
		if _, err := tx.Exec(`DELETE FROM sessions`); err != nil {
			return 0, err
		}
	}
	if stagedHasInstallTokens {
		if _, err := tx.Exec(`DELETE FROM install_tokens`); err != nil {
			return 0, err
		}
	}
	if stagedHasEnrollments {
		if _, err := tx.Exec(`DELETE FROM agent_enrollments`); err != nil {
			return 0, err
		}
	}

	if len(carry.passkeyCounters) > 0 {
		if !stagedHasCounters {
			if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS passkey_counters (
				user_uuid varchar(36),
				key_digest varchar(64),
				counter integer,
				PRIMARY KEY (user_uuid, key_digest)
			)`); err != nil {
				return 0, err
			}
		}
		for k, counter := range carry.passkeyCounters {
			if stagedHasUsers {
				var exists int
				if err := tx.QueryRow(`SELECT 1 FROM users WHERE uuid = ? LIMIT 1`, k.userUUID).Scan(&exists); err == sql.ErrNoRows {
					continue
				} else if err != nil {
					return 0, err
				}
			}
			if _, err := tx.Exec(`INSERT OR IGNORE INTO passkey_counters (user_uuid, key_digest, counter) VALUES (?, ?, ?)`, k.userUUID, k.keyDigest, counter); err != nil {
				return 0, err
			}
			if _, err := tx.Exec(`UPDATE passkey_counters SET counter = ? WHERE user_uuid = ? AND key_digest = ? AND counter < ?`, counter, k.userUUID, k.keyDigest, counter); err != nil {
				return 0, err
			}
		}
	}

	if len(carry.passkeyRevocations) > 0 {
		if !stagedHasRevocations {
			if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS passkey_revocations (
				credential_hash char(64) PRIMARY KEY,
				user_uuid varchar(36) NOT NULL,
				rp_id varchar(253) NOT NULL,
				revoked_at datetime NOT NULL
			)`); err != nil {
				return 0, err
			}
		}
		for _, rev := range carry.passkeyRevocations {
			if _, err := tx.Exec(`INSERT OR IGNORE INTO passkey_revocations (credential_hash, user_uuid, rp_id, revoked_at) VALUES (?, ?, ?, ?)`, rev.credentialHash, rev.userUUID, rev.rpID, rev.revokedAt); err != nil {
				return 0, err
			}
			if stagedHasPasskeys {
				if _, err := tx.Exec(`DELETE FROM user_passkeys WHERE credential_hash = ?`, rev.credentialHash); err != nil {
					return 0, err
				}
			}
		}
	}

	if stagedHasAPITokens && len(carry.revokedAPITokens) > 0 {
		for tokenID, revokedAt := range carry.revokedAPITokens {
			if _, err := tx.Exec(`UPDATE api_tokens SET revoked_at = COALESCE(revoked_at, ?) WHERE id = ?`, revokedAt, tokenID); err != nil {
				return 0, err
			}
		}
	}

	if len(carry.twoFactorCounters) > 0 {
		if !stagedHasTwoFactorCounters {
			if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS two_factor_counters (
				uuid varchar(36),
				counter integer,
				used_at datetime,
				expires_at datetime,
				PRIMARY KEY (uuid, counter)
			)`); err != nil {
				return 0, err
			}
		}
		for _, r := range carry.twoFactorCounters {
			if stagedHasUsers {
				var exists int
				if err := tx.QueryRow(`SELECT 1 FROM users WHERE uuid = ? LIMIT 1`, r.uuid).Scan(&exists); err == sql.ErrNoRows {
					continue
				} else if err != nil {
					return 0, err
				}
			}
			if _, err := tx.Exec(`INSERT OR IGNORE INTO two_factor_counters (uuid, counter, used_at, expires_at) VALUES (?, ?, ?, ?)`, r.uuid, r.counter, r.usedAt, r.expiresAt); err != nil {
				return 0, err
			}
		}
	}

	if stagedHasAgentCreds && len(carry.agentRevocations) > 0 {
		for _, rev := range carry.agentRevocations {
			reason := rev.revokedReason
			if reason == "" {
				reason = "revoked before restore"
			}
			if _, err := tx.Exec(`UPDATE agent_credentials
				SET revoked_at = COALESCE(revoked_at, ?),
				    revoked_reason = CASE WHEN COALESCE(revoked_reason, '') = '' THEN ? ELSE revoked_reason END,
				    refresh_recovery_sealed = '',
				    refresh_recovery_request = '',
				    refresh_recovery_until = NULL
				WHERE client = ? AND (agent_public_key = ? OR ? = '')`,
				rev.revokedAt, reason, rev.client, rev.publicKey, rev.publicKey,
			); err != nil {
				return 0, err
			}
		}
	}

	if stagedHasAgentCreds && len(carry.agentRefreshUses) > 0 {
		if !stagedHasRefreshUses {
			if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS agent_refresh_uses (
				token_hash varchar(64) PRIMARY KEY,
				client varchar(36) NOT NULL,
				request_hash varchar(64),
				generation integer,
				consumed_at datetime,
				expires_at datetime
			)`); err != nil {
				return 0, err
			}
		}
		for _, use := range carry.agentRefreshUses {
			var exists int
			if err := tx.QueryRow(`SELECT 1 FROM agent_credentials WHERE client = ? LIMIT 1`, use.client).Scan(&exists); err == sql.ErrNoRows {
				continue
			} else if err != nil {
				return 0, err
			}
			if _, err := tx.Exec(`INSERT OR IGNORE INTO agent_refresh_uses (token_hash, client, request_hash, generation, consumed_at, expires_at) VALUES (?, ?, ?, ?, ?, ?)`,
				use.tokenHash, use.client, use.requestHash, use.generation, use.consumedAt, use.expiresAt,
			); err != nil {
				return 0, err
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	if _, err := stagedDB.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return 0, fmt.Errorf("checkpoint staged database: %w", err)
	}
	stagedClosed = true
	if err := stagedDB.Close(); err != nil {
		return 0, fmt.Errorf("close staged database after checkpoint: %w", err)
	}
	_ = os.Remove(stagedPath + "-wal")
	_ = os.Remove(stagedPath + "-shm")
	return newEpoch, nil
}
