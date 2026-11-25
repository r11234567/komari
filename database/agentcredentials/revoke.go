package agentcredentials

import (
	"context"
	"strings"
	"time"

	"github.com/komari-monitor/komari/database/models"
	"gorm.io/gorm"
)

// Revoke marks credentials invalid in the caller's transaction.
func Revoke(db *gorm.DB, agentID, reason string) error {
	return revoke(db.Model(&models.AgentCredential{}).Where("client = ?", agentID), reason)
}

// RevokeOrphans invalidates credentials left behind by earlier node deletions.
func RevokeOrphans(ctx context.Context, db *gorm.DB) error {
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		clients := tx.Model(&models.Client{}).Select("uuid")
		orphans := tx.Model(&models.AgentCredential{}).Where("revoked_at IS NULL AND client NOT IN (?)", clients)
		if err := revoke(orphans, "node deleted"); err != nil {
			return err
		}
		return tx.Where("client <> '' AND client NOT IN (?)", clients).Delete(&models.AgentEnrollment{}).Error
	})
}

func revoke(credentials *gorm.DB, reason string) error {
	now := time.Now().UTC()
	reason = strings.TrimSpace(reason)
	if len(reason) > 255 {
		reason = reason[:255]
	}
	return credentials.Updates(map[string]any{
		"revoked_at": now, "revoked_reason": reason, "updated_at": now,
		"refresh_recovery_sealed": "", "refresh_recovery_until": nil, "refresh_recovery_request": "",
	}).Error
}
