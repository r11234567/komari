package enrollment

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/komari-monitor/komari/database/models"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestCredentialLifecycleAbsoluteDeadlineNeverSlides(t *testing.T) {
	db, _, _, now := rotationFixture(t)
	var issued Issued
	key := base64.StdEncoding.EncodeToString(make([]byte, 32))
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		var err error
		issued, err = issueLocked(tx, uuid.NewString(), key, "key", 1, []string{"report"}, now)
		return err
	}))
	require.Equal(t, now.Add(7*credentialDay), issued.RefreshTokenExpiresAt)
	var record models.AgentCredential
	require.NoError(t, db.First(&record, "client = ?", issued.AgentID).Error)
	absolute := now.Add(180 * credentialDay)
	require.True(t, record.EnrollmentExpiresAt.Equal(absolute))
	token := issued.RefreshToken
	for day := 6; day <= 174; day += 6 {
		clock := now.Add(time.Duration(day) * credentialDay)
		generation := record.Generation
		next, err := rotateVerifiedDB(context.Background(), db, record, token, uuid.NewString(), &generation, 2, func() time.Time { return clock })
		require.NoError(t, err)
		require.False(t, next.RefreshTokenExpiresAt.After(absolute))
		require.NoError(t, db.First(&record, "client = ?", issued.AgentID).Error)
		require.True(t, record.EnrollmentExpiresAt.Equal(absolute), "routine RT rotation slid the human authorization deadline")
		token = next.RefreshToken
	}
	near := absolute.Add(-30 * time.Minute)
	final, err := rotateVerifiedDB(context.Background(), db, record, token, uuid.NewString(), &record.Generation, 2, func() time.Time { return near })
	require.NoError(t, err)
	require.True(t, final.AccessTokenExpiresAt.Equal(absolute), "AT must not outlive authorization")
	require.True(t, final.RefreshTokenExpiresAt.Equal(absolute))
	require.NoError(t, db.First(&record, "client = ?", record.Client).Error)
	_, err = rotateVerifiedDB(context.Background(), db, record, final.RefreshToken, uuid.NewString(), &record.Generation, 2, func() time.Time { return absolute })
	require.ErrorIs(t, err, ErrCredentialRejected)
}

func TestCredentialLifecycleIdleExpiryAndMigrationAreBounded(t *testing.T) {
	db, token, record, now := rotationFixture(t)
	old := now.Add(-400 * credentialDay)
	require.NoError(t, db.Model(&record).Update("created_at", old).Error)
	require.NoError(t, initializeLifecycleDB(context.Background(), db, now))
	require.NoError(t, db.First(&record, "client = ?", record.Client).Error)
	fixed := now.Add(30 * credentialDay)
	require.True(t, record.EnrollmentExpiresAt.Equal(fixed))
	require.True(t, record.MigrationGraceUntil.Equal(fixed))
	require.Nil(t, record.LastRefreshAt, "migration must not invent a successful renewal")
	require.NoError(t, initializeLifecycleDB(context.Background(), db, now.Add(10*credentialDay)))
	var after models.AgentCredential
	require.NoError(t, db.First(&after, "client = ?", record.Client).Error)
	require.True(t, after.EnrollmentExpiresAt.Equal(fixed), "restart renewed migration grace")
	// The same RT is denied at the exact idle boundary.
	expiry := now.Add(7 * credentialDay)
	require.NoError(t, db.Model(&record).Updates(map[string]any{"refresh_expires_at": expiry, "migration_grace_until": nil}).Error)
	_, err := rotateVerifiedDB(context.Background(), db, record, token, uuid.NewString(), nil, 2, func() time.Time { return expiry })
	require.ErrorIs(t, err, ErrCredentialRejected)
	// A revoked or already expired old enrollment is never revived by migration.
	expired := models.AgentCredential{Client: uuid.NewString(), RefreshTokenHash: hashToken("expired"), RefreshExpiresAt: now.Add(-time.Hour), RevokedAt: &old, AgentPublicKey: "synthetic"}
	require.NoError(t, db.Create(&expired).Error)
	require.NoError(t, initializeLifecycleDB(context.Background(), db, now))
	var revoked models.AgentCredential
	require.NoError(t, db.First(&revoked, "client = ?", expired.Client).Error)
	require.NotNil(t, revoked.RevokedAt)
	require.False(t, revoked.RefreshExpiresAt.After(now))
}

func TestCredentialLifecycleRecoveryCleanupKeepsReuseDetection(t *testing.T) {
	db, token, record, now := rotationFixture(t)
	_, err := rotateVerifiedDB(context.Background(), db, record, token, uuid.NewString(), nil, 2, func() time.Time { return now })
	require.NoError(t, err)
	require.NoError(t, cleanupLifecycleDB(context.Background(), db, now.Add(RefreshRecoveryWindow)))
	var current models.AgentCredential
	require.NoError(t, db.First(&current, "client = ?", record.Client).Error)
	require.Empty(t, current.RefreshRecoverySealed)
	var count int64
	require.NoError(t, db.Model(&models.AgentRefreshUse{}).Count(&count).Error)
	require.EqualValues(t, 1, count)
	_, err = rotateVerifiedDB(context.Background(), db, record, token, uuid.NewString(), nil, 2, func() time.Time { return now.Add(2 * time.Minute) })
	require.ErrorIs(t, err, ErrRefreshReuse)
	require.NoError(t, cleanupLifecycleDB(context.Background(), db, now.Add(181*credentialDay)))
	require.NoError(t, db.Model(&models.AgentRefreshUse{}).Count(&count).Error)
	require.Zero(t, count)
}

func TestCredentialLifecycleReapprovalRequiresUnchangedUnrevokedKey(t *testing.T) {
	db, token, record, now := rotationFixture(t)
	key := base64.StdEncoding.EncodeToString(make([]byte, 32))
	fingerprint := credentialFingerprint(key)
	require.NoError(t, db.Model(&record).Update("agent_public_key", key).Error)
	require.ErrorIs(t, reapproveCredentialDB(context.Background(), db, record.Client, 99, fingerprint, "admin", now), ErrRefreshConflict)
	require.ErrorIs(t, reapproveCredentialDB(context.Background(), db, record.Client, 0, "incorrect", "admin", now), ErrRefreshConflict)
	require.NoError(t, reapproveCredentialDB(context.Background(), db, record.Client, 0, fingerprint, "admin", now))
	var approved models.AgentCredential
	require.NoError(t, db.First(&approved, "client = ?", record.Client).Error)
	require.True(t, approved.EnrollmentExpiresAt.Equal(now.Add(180*credentialDay)))
	require.True(t, approved.RefreshExpiresAt.Equal(now.Add(7*credentialDay)))
	require.Equal(t, "admin", approved.LastApprovedBy)
	_, err := rotateVerifiedDB(context.Background(), db, approved, token, uuid.NewString(), nil, 2, func() time.Time { return now })
	require.NoError(t, err)
	require.ErrorContains(t, reapproveCredentialDB(context.Background(), db, record.Client, 1, fingerprint, "admin", now), "recovering")
	require.NoError(t, db.Model(&approved).Update("revoked_at", now).Error)
	require.ErrorContains(t, reapproveCredentialDB(context.Background(), db, record.Client, 1, fingerprint, "admin", now.Add(time.Minute)), "new device enrollment")
}

func TestCredentialLifecycleStatusRedactionAndPolicyBounds(t *testing.T) {
	policy := defaultCredentialPolicy()
	require.NoError(t, validateCredentialPolicy(policy))
	for _, days := range []int{0, 31, 999} {
		bad := policy
		bad.IdleDays = days
		require.Error(t, validateCredentialPolicy(bad))
	}
	now := time.Now().UTC()
	absolute := now.Add(10 * credentialDay)
	row := models.AgentCredential{Client: "synthetic", AgentPublicKey: base64.StdEncoding.EncodeToString(make([]byte, 32)), AccessTokenHash: "SECRET-AT-HASH", RefreshTokenHash: "SECRET-RT-HASH", RefreshRecoverySealed: "SECRET-CIPHERTEXT", RefreshExpiresAt: now.Add(7 * credentialDay), EnrollmentExpiresAt: &absolute}
	status := credentialStatus(row, "node", policy, now)
	require.Equal(t, "expiring", status.Status)
	raw, err := json.Marshal(status)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "SECRET")
	require.NotContains(t, strings.ToLower(string(raw)), "token")
	require.Len(t, status.Fingerprint, 43)
}
