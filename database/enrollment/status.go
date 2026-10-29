package enrollment

import (
	"context"
	"time"

	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/models"
)

type CredentialStatus struct {
	AgentID             string     `json:"agent_id"`
	Name                string     `json:"name"`
	Generation          uint64     `json:"generation"`
	Protocol            int        `json:"protocol"`
	Fingerprint         string     `json:"fingerprint"`
	Status              string     `json:"status"`
	AccessExpiresAt     time.Time  `json:"access_expires_at"`
	IdleExpiresAt       time.Time  `json:"idle_expires_at"`
	AbsoluteExpiresAt   *time.Time `json:"absolute_expires_at"`
	LastRefreshAt       *time.Time `json:"last_refresh_at"`
	MigrationGraceUntil *time.Time `json:"migration_grace_until"`
	RecoveryUntil       *time.Time `json:"recovery_until"`
	LastApprovedAt      *time.Time `json:"last_approved_at"`
	CanReapprove        bool       `json:"can_reapprove"`
}

func credentialStatus(row models.AgentCredential, name string, policy models.AgentCredentialPolicy, now time.Time) CredentialStatus {
	status := "active"
	switch {
	case row.RevokedAt != nil:
		status = "revoked"
	case !now.Before(row.RefreshExpiresAt) || (row.EnrollmentExpiresAt != nil && !now.Before(*row.EnrollmentExpiresAt)):
		status = "expired"
	case row.EnrollmentExpiresAt != nil && row.EnrollmentExpiresAt.Sub(now) <= time.Duration(policy.WarningDays)*credentialDay:
		status = "expiring"
	case row.MigrationGraceUntil != nil && now.Before(*row.MigrationGraceUntil):
		status = "migration"
	}
	return CredentialStatus{AgentID: row.Client, Name: name, Generation: row.Generation, Protocol: row.RefreshProtocol, Fingerprint: credentialFingerprint(row.AgentPublicKey), Status: status, AccessExpiresAt: row.AccessExpiresAt, IdleExpiresAt: row.RefreshExpiresAt, AbsoluteExpiresAt: row.EnrollmentExpiresAt, LastRefreshAt: row.LastRefreshAt, MigrationGraceUntil: row.MigrationGraceUntil, RecoveryUntil: row.RefreshRecoveryUntil, LastApprovedAt: row.LastApprovedAt, CanReapprove: row.RevokedAt == nil && (row.RefreshRecoveryUntil == nil || !now.Before(*row.RefreshRecoveryUntil))}
}
func CredentialStatuses(ctx context.Context, page int) ([]CredentialStatus, int64, error) {
	db := dbcore.GetDBInstance().WithContext(ctx)
	policy, err := loadCredentialPolicy(db)
	if err != nil {
		return nil, 0, err
	}
	var count int64
	if err = db.Model(&models.AgentCredential{}).Count(&count).Error; err != nil {
		return nil, 0, err
	}
	var rows []models.AgentCredential
	fields := []string{"client", "generation", "refresh_protocol", "agent_public_key", "access_expires_at", "refresh_expires_at", "enrollment_expires_at", "last_refresh_at", "migration_grace_until", "refresh_recovery_until", "last_approved_at", "revoked_at"}
	if err = db.Select(fields).Order("client").Offset((page - 1) * 100).Limit(100).Find(&rows).Error; err != nil {
		return nil, 0, err
	}
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.Client)
	}
	var clients []models.Client
	if len(ids) > 0 {
		if err = db.Select("uuid", "name").Where("uuid IN ?", ids).Find(&clients).Error; err != nil {
			return nil, 0, err
		}
	}
	names := map[string]string{}
	for _, c := range clients {
		names[c.UUID] = c.Name
	}
	result := make([]CredentialStatus, 0, len(rows))
	now := time.Now().UTC()
	for _, row := range rows {
		result = append(result, credentialStatus(row, names[row.Client], policy, now))
	}
	return result, count, nil
}
