package enrollment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/utils"
	"github.com/komari-monitor/komari/utils/secureconfig"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const RefreshRecoveryWindow = time.Minute
const AccessHandoffWindow = time.Minute

var ErrCredentialRejected = errors.New("credential rejected")
var ErrRefreshConflict = errors.New("credential generation conflict")
var ErrRefreshReuse = errors.New("consumed refresh credential reused; credential chain revoked")

// LookupRefresh locates key material only. It authorizes nothing. The caller
// must prove possession of the enrolled private key before any state mutation,
// including reuse detection and revocation. Historical hashes permit detecting
// authenticated reuse without storing old tokens or allowing them to refresh.
func LookupRefresh(ctx context.Context, token string) (models.AgentCredential, error) {
	return lookupRefresh(dbcore.GetDBInstance().WithContext(ctx), hashToken(token))
}

func lookupRefresh(db *gorm.DB, digest string) (models.AgentCredential, error) {
	var row models.AgentCredential
	if digest == "" {
		return row, ErrCredentialRejected
	}
	err := db.Where("refresh_token_hash = ? OR previous_refresh_hash = ?", digest, digest).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		var used models.AgentRefreshUse
		err = db.Where("token_hash = ?", digest).First(&used).Error
		if err == nil {
			err = db.Where("client = ?", used.Client).First(&row).Error
		}
	}
	return row, err
}

// RotateVerified must only be called after a fresh signature binds token,
// client, request ID and expected generation. A row lock plus CAS works across
// panel processes; SQLite's competing writers are retried without global locks.
func RotateVerified(ctx context.Context, record models.AgentCredential, token, requestID string, expected *uint64, protocol int) (Issued, error) {
	return rotateVerifiedDB(ctx, dbcore.GetDBInstance(), record, token, requestID, expected, protocol, time.Now)
}

func rotateVerifiedDB(ctx context.Context, db *gorm.DB, record models.AgentCredential, token, requestID string, expected *uint64, protocol int, clock func() time.Time) (Issued, error) {
	var issued Issued
	digest := hashToken(token)
	if digest == "" || record.Client == "" || requestID == "" || (protocol != 1 && protocol != 2) {
		return issued, ErrCredentialRejected
	}
	requestHash := hashToken(requestID)
	for attempt := 0; attempt < 6; attempt++ {
		var outcome error
		err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			now := clock().UTC()
			var cur models.AgentCredential
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("client = ?", record.Client).First(&cur).Error; err != nil {
				return err
			}
			if cur.RevokedAt != nil {
				return fmt.Errorf("%w: machine revoked", ErrCredentialRejected)
			}
			// A proof verified against a key read before concurrent reenrollment must
			// never rotate the replacement identity's credentials.
			if cur.AgentPublicKey != record.AgentPublicKey {
				return ErrCredentialRejected
			}
			if cur.RefreshProtocol >= 2 && protocol < 2 {
				return fmt.Errorf("%w: refresh protocol downgrade", ErrCredentialRejected)
			}
			if !now.Before(cur.RefreshExpiresAt) {
				return fmt.Errorf("%w: refresh expired", ErrCredentialRejected)
			}
			if digest != cur.RefreshTokenHash {
				if digest == cur.PreviousRefreshHash && cur.RefreshRecoveryUntil != nil && now.Before(*cur.RefreshRecoveryUntil) {
					if cur.RefreshRecoveryRequest != requestHash {
						return ErrRefreshConflict
					}
					plaintext, err := secureconfig.DecryptString(cur.RefreshRecoverySealed)
					if err != nil {
						return errors.New("refresh recovery unavailable")
					}
					if err := json.Unmarshal([]byte(plaintext), &issued); err != nil {
						return errors.New("refresh recovery invalid")
					}
					if issued.AgentID != cur.Client || issued.Generation != cur.Generation || hashToken(issued.RefreshToken) != cur.RefreshTokenHash || hashToken(issued.AccessToken) != cur.AccessTokenHash || !issued.AccessTokenExpiresAt.Equal(cur.AccessExpiresAt) || !issued.RefreshTokenExpiresAt.Equal(cur.RefreshExpiresAt) || issued.RequestID != requestID {
						return errors.New("refresh recovery binding mismatch")
					}
					if expected != nil && *expected+1 != cur.Generation {
						return ErrRefreshConflict
					}
					return nil
				}
				var consumed models.AgentRefreshUse
				err := tx.Where("token_hash = ? AND client = ?", digest, cur.Client).First(&consumed).Error
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return ErrCredentialRejected
				}
				if err != nil {
					return err
				}
				// Legacy clients retain compatibility without weakening already-upgraded
				// machines. A stale legacy request cannot mint another credential pair.
				if protocol < 2 {
					return ErrCredentialRejected
				}
				result := tx.Model(&models.AgentCredential{}).Where("client = ? AND revoked_at IS NULL", cur.Client).Updates(map[string]any{
					"revoked_at": now, "revoked_reason": "authenticated refresh reuse", "refresh_recovery_sealed": "", "refresh_recovery_until": nil, "updated_at": now,
				})
				if result.Error != nil {
					return result.Error
				}
				outcome = ErrRefreshReuse // Commit the revocation before returning error.
				return nil
			}
			if expected != nil && *expected != cur.Generation {
				return ErrRefreshConflict
			}
			if protocol == 2 && cur.RefreshProtocol >= 2 && expected == nil {
				return ErrRefreshConflict
			}
			access := utils.GenerateRandomString(43)
			refresh := utils.GenerateRandomString(43)
			if access == "" || refresh == "" {
				return errors.New("credential random generation failed")
			}
			until := now.Add(RefreshRecoveryWindow)
			if protocol == 1 {
				until = now.Add(refreshGracePeriod)
			}
			previousAccessUntil := now.Add(AccessHandoffWindow)
			if cur.AccessExpiresAt.Before(previousAccessUntil) {
				previousAccessUntil = cur.AccessExpiresAt
			}
			issued = Issued{AgentID: cur.Client, AccessToken: access, AccessTokenExpiresAt: now.Add(AccessTokenLifetime), RefreshToken: refresh,
				RefreshTokenExpiresAt: now.Add(RefreshTokenLifetime), Generation: cur.Generation + 1, RequestID: requestID, Scopes: decodeScopes(cur.Scopes)}
			plaintext, err := json.Marshal(issued)
			if err != nil {
				return err
			}
			sealed, err := secureconfig.EncryptString(string(plaintext))
			if err != nil {
				return errors.New("cannot seal refresh recovery")
			}
			used := models.AgentRefreshUse{TokenHash: digest, Client: cur.Client, Generation: cur.Generation, ConsumedAt: now, ExpiresAt: cur.RefreshExpiresAt}
			if err := tx.Create(&used).Error; err != nil {
				return err
			}
			result := tx.Model(&models.AgentCredential{}).Where("client = ? AND generation = ? AND refresh_token_hash = ? AND revoked_at IS NULL", cur.Client, cur.Generation, digest).Updates(map[string]any{
				"generation": issued.Generation, "refresh_protocol": protocol, "access_token_hash": hashToken(access), "access_expires_at": issued.AccessTokenExpiresAt,
				"refresh_token_hash": hashToken(refresh), "refresh_expires_at": issued.RefreshTokenExpiresAt,
				"previous_refresh_hash": digest, "previous_refresh_expires": until,
				"previous_access_hash": cur.AccessTokenHash, "previous_access_expires": previousAccessUntil,
				"refresh_recovery_request": requestHash, "refresh_recovery_sealed": sealed, "refresh_recovery_until": until, "updated_at": now,
			})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return ErrRefreshConflict
			}
			return nil
		})
		if err == nil {
			return issued, outcome
		}
		if ctx.Err() != nil {
			return Issued{}, ctx.Err()
		}
		// Retry only actual transaction contention, never validation/permission
		// failures. Backoff is bounded and context-cancellable.
		message := strings.ToLower(err.Error())
		retry := strings.Contains(message, "database is locked") || strings.Contains(message, "database table is locked") || strings.Contains(message, "deadlock") || strings.Contains(message, "serialization failure") || strings.Contains(message, "could not serialize")
		if !retry || attempt == 5 {
			return Issued{}, err
		}
		timer := time.NewTimer(time.Duration(attempt+1) * 10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return Issued{}, ctx.Err()
		case <-timer.C:
		}
	}
	return Issued{}, ErrRefreshConflict
}
