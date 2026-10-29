package enrollment

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"time"

	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/models"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const credentialDay = 24 * time.Hour

func defaultCredentialPolicy() models.AgentCredentialPolicy {
	return models.AgentCredentialPolicy{ID: 1, Revision: 1, IdleDays: 7, AbsoluteDays: 180, MigrationGraceDays: 30, WarningDays: 14}
}
func validateCredentialPolicy(policy models.AgentCredentialPolicy) error {
	if policy.IdleDays < 1 || policy.IdleDays > 30 || policy.AbsoluteDays < 30 || policy.AbsoluteDays > 365 || policy.AbsoluteDays < policy.IdleDays || policy.MigrationGraceDays < 1 || policy.MigrationGraceDays > 90 || policy.WarningDays < 1 || policy.WarningDays > 30 {
		return errors.New("idle days must be 1–30; absolute days 30–365 and >= idle; migration grace 1–90; warning 1–30")
	}
	return nil
}
func loadCredentialPolicy(tx *gorm.DB) (models.AgentCredentialPolicy, error) {
	var policy models.AgentCredentialPolicy
	err := tx.First(&policy, 1).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		policy = defaultCredentialPolicy()
		err = tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&policy).Error
		if err == nil {
			err = tx.First(&policy, 1).Error
		}
	}
	if err == nil {
		err = validateCredentialPolicy(policy)
	}
	return policy, err
}
func CredentialPolicy(ctx context.Context) (models.AgentCredentialPolicy, error) {
	return loadCredentialPolicy(dbcore.GetDBInstance().WithContext(ctx))
}
func SaveCredentialPolicy(ctx context.Context, policy models.AgentCredentialPolicy) (models.AgentCredentialPolicy, error) {
	if err := validateCredentialPolicy(policy); err != nil {
		return policy, err
	}
	db := dbcore.GetDBInstance().WithContext(ctx)
	current, err := loadCredentialPolicy(db)
	if err != nil {
		return policy, err
	}
	if current.Revision != policy.Revision {
		return policy, ErrRefreshConflict
	}
	result := db.Model(&models.AgentCredentialPolicy{}).Where("id = 1 AND revision = ?", policy.Revision).Updates(map[string]any{
		"revision": policy.Revision + 1, "idle_days": policy.IdleDays, "absolute_days": policy.AbsoluteDays, "migration_grace_days": policy.MigrationGraceDays, "warning_days": policy.WarningDays, "updated_at": time.Now().UTC(),
	})
	if result.Error != nil {
		return policy, result.Error
	}
	if result.RowsAffected != 1 {
		return policy, ErrRefreshConflict
	}
	return loadCredentialPolicy(db)
}
func minDeadline(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
func idleDeadline(row models.AgentCredential, policy models.AgentCredentialPolicy, now time.Time) time.Time {
	deadline := now.Add(time.Duration(policy.IdleDays) * credentialDay)
	if row.MigrationGraceUntil != nil && row.MigrationGraceUntil.After(deadline) {
		deadline = *row.MigrationGraceUntil
	}
	if row.EnrollmentExpiresAt != nil {
		deadline = minDeadline(deadline, *row.EnrollmentExpiresAt)
	}
	return deadline
}

// A once-only migration extends no already-expired RT and never clears a
// revocation. Very old enrollments receive an explicit fixed migration grace.
func migrateCredential(tx *gorm.DB, row *models.AgentCredential, policy models.AgentCredentialPolicy, now time.Time) error {
	if row.LifecycleVersion != 0 {
		return nil
	}
	started := row.CreatedAt
	if started.IsZero() {
		started = now
	}
	grace := now.Add(time.Duration(policy.MigrationGraceDays) * credentialDay)
	absolute := started.Add(time.Duration(policy.AbsoluteDays) * credentialDay)
	if absolute.Before(grace) {
		absolute = grace
	}
	row.EnrollmentExpiresAt = &absolute
	row.MigrationGraceUntil = &grace
	row.LastRefreshAt = nil
	row.LifecycleVersion = 1
	expiry := row.RefreshExpiresAt
	if expiry.After(now) {
		expiry = minDeadline(expiry, idleDeadline(*row, policy, now))
	}
	result := tx.Model(&models.AgentCredential{}).Where("client = ? AND lifecycle_version = 0", row.Client).Updates(map[string]any{
		"enrollment_expires_at": absolute, "migration_grace_until": grace, "last_refresh_at": nil, "lifecycle_version": 1, "refresh_expires_at": expiry,
	})
	if result.Error == nil && result.RowsAffected == 1 {
		row.RefreshExpiresAt = expiry
	}
	return result.Error
}
func initializeLifecycleDB(ctx context.Context, db *gorm.DB, now time.Time) error {
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		policy, err := loadCredentialPolicy(tx)
		if err != nil {
			return err
		}
		for {
			var rows []models.AgentCredential
			if err = tx.Where("lifecycle_version = 0").Limit(500).Find(&rows).Error; err != nil {
				return err
			}
			if len(rows) == 0 {
				return nil
			}
			for i := range rows {
				if err = migrateCredential(tx, &rows[i], policy, now); err != nil {
					return err
				}
			}
		}
	})
}
func InitializeLifecycle(ctx context.Context) error {
	return initializeLifecycleDB(ctx, dbcore.GetDBInstance(), time.Now().UTC())
}
func cleanupLifecycleDB(ctx context.Context, db *gorm.DB, now time.Time) error {
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&models.AgentCredential{}).Where("refresh_recovery_until <= ? AND refresh_recovery_sealed <> ''", now).Updates(map[string]any{"refresh_recovery_sealed": "", "refresh_recovery_request": ""}).Error; err != nil {
			return err
		}
		return tx.Where("expires_at <= ?", now).Delete(&models.AgentRefreshUse{}).Error
	})
}
func CleanupLifecycle(ctx context.Context) error {
	return cleanupLifecycleDB(ctx, dbcore.GetDBInstance(), time.Now().UTC())
}

// Reapproval does not clear compromise/revocation, rotate tokens, change keys
// or invalidate an in-flight recovery response. Those require new enrollment.
func ReapproveCredential(ctx context.Context, agentID string, expected uint64, fingerprint, actor string) error {
	return reapproveCredentialDB(ctx, dbcore.GetDBInstance(), agentID, expected, fingerprint, actor, time.Now().UTC())
}
func reapproveCredentialDB(ctx context.Context, db *gorm.DB, agentID string, expected uint64, fingerprint, actor string, now time.Time) error {
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row models.AgentCredential
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&row, "client = ?", agentID).Error; err != nil {
			return err
		}
		if row.Generation != expected || credentialFingerprint(row.AgentPublicKey) != fingerprint {
			return ErrRefreshConflict
		}
		if row.RevokedAt != nil {
			return errors.New("revoked identity requires new device enrollment and a new key")
		}
		if row.RefreshRecoveryUntil != nil && now.Before(*row.RefreshRecoveryUntil) {
			return errors.New("credential rotation is recovering; retry after its recovery window")
		}
		policy, err := loadCredentialPolicy(tx)
		if err != nil {
			return err
		}
		absolute := now.Add(time.Duration(policy.AbsoluteDays) * credentialDay)
		idle := minDeadline(now.Add(time.Duration(policy.IdleDays)*credentialDay), absolute)
		result := tx.Model(&models.AgentCredential{}).Where("client = ? AND generation = ? AND revoked_at IS NULL", agentID, expected).Updates(map[string]any{
			"enrollment_expires_at": absolute, "refresh_expires_at": idle, "migration_grace_until": nil, "last_approved_at": now, "last_approved_by": actor, "lifecycle_version": 1,
		})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrRefreshConflict
		}
		return nil
	})
}
func credentialFingerprint(encoded string) string {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
