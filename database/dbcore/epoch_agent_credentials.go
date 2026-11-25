package dbcore

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"time"

	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/utils/secureconfig"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func sameRestoreDBPath(first, second string) bool {
	a, errA := filepath.Abs(first)
	b, errB := filepath.Abs(second)
	return errA == nil && errB == nil && a == b
}

func restoreDBModel(db *sql.DB) (*gorm.DB, error) {
	return gorm.Open(sqlite.New(sqlite.Config{Conn: db}), &gorm.Config{DisableAutomaticPing: true})
}

func readLiveAgentCredentials(db *sql.DB) (map[string]models.AgentCredential, error) {
	credentials := make(map[string]models.AgentCredential)
	present, err := sqliteTableExists(db, "agent_credentials")
	if err != nil || !present {
		return credentials, err
	}
	orm, err := restoreDBModel(db)
	if err != nil {
		return nil, err
	}
	var records []models.AgentCredential
	if err := orm.Find(&records).Error; err != nil {
		return nil, err
	}
	for _, record := range records {
		credentials[record.Client] = record
	}
	return credentials, nil
}

func (carry restoreSecurityCarryover) rewrapAgentRecovery(liveDir, stageDir string) error {
	now := time.Now()
	for id, credential := range carry.agentCredentials {
		if credential.RevokedAt != nil {
			continue
		}
		if credential.RefreshRecoveryUntil == nil || !now.Before(*credential.RefreshRecoveryUntil) {
			credential.RefreshRecoverySealed = ""
			credential.RefreshRecoveryRequest = ""
			credential.RefreshRecoveryUntil = nil
		}
		sealed, err := secureconfig.RewrapForRestore(credential.RefreshRecoverySealed, liveDir, stageDir)
		if err != nil {
			return fmt.Errorf("preserve active agent refresh recovery: %w", err)
		}
		credential.RefreshRecoverySealed = sealed
		carry.agentCredentials[id] = credential
	}
	for i, key := range carry.controlPlanePrivate {
		sealed, err := secureconfig.RewrapForRestore(key.Sealed, liveDir, stageDir)
		if err != nil {
			return fmt.Errorf("preserve live control-plane signing key: %w", err)
		}
		carry.controlPlanePrivate[i].Sealed = sealed
	}
	return nil
}

func readLiveControlPlaneKeys(db *sql.DB, carry *restoreSecurityCarryover) error {
	public, err := sqliteTableExists(db, "control_plane_keys")
	if err != nil || !public {
		return err
	}
	private, err := sqliteTableExists(db, "control_plane_private_keys")
	if err != nil || !private {
		return err
	}
	orm, err := restoreDBModel(db)
	if err != nil {
		return err
	}
	if err := orm.Find(&carry.controlPlaneKeys).Error; err != nil {
		return err
	}
	return orm.Find(&carry.controlPlanePrivate).Error
}

func reconcileLiveControlPlaneKeys(tx *sql.Tx, carry restoreSecurityCarryover) error {
	if len(carry.controlPlaneKeys) == 0 {
		return nil
	}
	if _, err := tx.Exec(`DELETE FROM control_plane_private_keys`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM control_plane_keys`); err != nil {
		return err
	}
	for _, key := range carry.controlPlaneKeys {
		if _, err := tx.Exec(`INSERT INTO control_plane_keys
			(key_id, algorithm, public_key, not_before, not_after, created_at, updated_at, trust_revision, rotation_proof)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, key.KeyID, key.Algorithm, key.PublicKey,
			key.NotBefore, key.NotAfter, key.CreatedAt, key.UpdatedAt, max(1, key.TrustRevision), key.RotationProof); err != nil {
			return err
		}
	}
	for _, key := range carry.controlPlanePrivate {
		if _, err := tx.Exec(`INSERT INTO control_plane_private_keys (key_id, sealed, created_at)
			VALUES (?, ?, ?)`, key.KeyID, key.Sealed, key.CreatedAt); err != nil {
			return err
		}
	}
	return nil
}

func prepareStagedAgentCredentials(db *sql.DB) error {
	orm, err := restoreDBModel(db)
	if err != nil {
		return err
	}
	return orm.AutoMigrate(&models.AgentCredential{})
}

func reconcileLiveAgentCredentials(tx *sql.Tx, current map[string]models.AgentCredential) error {
	rows, err := tx.Query(`SELECT client, agent_public_key FROM agent_credentials WHERE revoked_at IS NULL`)
	if err != nil {
		return err
	}
	type stagedCredential struct{ id, key string }
	var staged []stagedCredential
	for rows.Next() {
		var record stagedCredential
		if err := rows.Scan(&record.id, &record.key); err != nil {
			_ = rows.Close()
			return err
		}
		staged = append(staged, record)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	now := time.Now().UTC()
	for _, record := range staged {
		live, exists := current[record.id]
		if !exists || live.AgentPublicKey != record.key || live.RevokedAt != nil {
			if _, err := tx.Exec(`UPDATE agent_credentials SET revoked_at = ?, revoked_reason = ?,
				refresh_recovery_sealed = '', refresh_recovery_request = '', refresh_recovery_until = NULL
				WHERE client = ?`, now, "enrollment absent or superseded at restore time", record.id); err != nil {
				return err
			}
			continue
		}
		if live.DPoPJWTRequired {
			if _, err := tx.Exec(`UPDATE agent_credentials SET dpop_jwt_required = 1 WHERE client = ?`, record.id); err != nil {
				return err
			}
		}
		if live.RefreshTokenHash == "" {
			// Older credential rows may predate refresh support. Preserve their
			// enrollment metadata and keep them subject to the epoch check.
			continue
		}
		// Preserve live generations and authorization deadlines. Keep the epoch
		// stale so the next request needs an actual signed refresh.
		_, err := tx.Exec(`UPDATE agent_credentials SET
			lifecycle_version = ?, enrollment_expires_at = ?, last_refresh_at = ?, migration_grace_until = ?,
			last_approved_at = ?, last_approved_by = ?, generation = ?, refresh_protocol = ?,
			previous_access_hash = ?, previous_access_expires = ?, refresh_recovery_sealed = ?,
			refresh_recovery_request = ?, refresh_recovery_until = ?, access_token_hash = ?, refresh_token_hash = ?,
			previous_refresh_hash = ?, previous_refresh_expires = ?, access_expires_at = ?, refresh_expires_at = ?,
			agent_key_id = ?, key_algorithm = ?, scopes = ?, security_epoch = ?, updated_at = ? WHERE client = ?`,
			live.LifecycleVersion, live.EnrollmentExpiresAt, live.LastRefreshAt, live.MigrationGraceUntil,
			live.LastApprovedAt, live.LastApprovedBy, live.Generation, live.RefreshProtocol,
			live.PreviousAccessHash, live.PreviousAccessExpires, live.RefreshRecoverySealed,
			live.RefreshRecoveryRequest, live.RefreshRecoveryUntil, live.AccessTokenHash, live.RefreshTokenHash,
			live.PreviousRefreshHash, live.PreviousRefreshExpires, live.AccessExpiresAt, live.RefreshExpiresAt,
			live.AgentKeyID, live.KeyAlgorithm, live.Scopes, 1, live.UpdatedAt, record.id)
		if err != nil {
			return err
		}
	}
	return nil
}
