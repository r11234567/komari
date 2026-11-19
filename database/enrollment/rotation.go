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

const RefreshRecoveryWindow = 24 * time.Hour
const AccessHandoffWindow = time.Minute

var ErrCredentialRejected = errors.New("credential rejected")
var ErrRefreshConflict = errors.New("credential generation conflict")
var ErrRefreshReuse = errors.New("consumed refresh credential reused; credential chain revoked")
var ErrRefreshRecoveryRequired = errors.New("refresh recovery window expired for this operation; reauthenticate machine")

// SealRefreshFunc encodes and signs the issued credential response before the
// database transaction commits, so a signing failure never consumes the old RT
// and lost-response recovery replays the exact signed bytes without re-signing.
type SealRefreshFunc func(Issued) ([]byte, error)

type sealedRefreshRecord struct {
	Issued         Issued `json:"issued"`
	SignedResponse []byte `json:"signed_response,omitempty"`
}

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
	issued, _, err := rotateVerifiedDBWithSeal(ctx, dbcore.GetDBInstance(), record, token, requestID, expected, protocol, time.Now, nil)
	return issued, err
}

// RotateVerifiedWithSeal runs RotateVerified and seals the full signed response
// atomically inside the same database commit.
func RotateVerifiedWithSeal(ctx context.Context, record models.AgentCredential, token, requestID string, expected *uint64, protocol int, seal SealRefreshFunc) (Issued, []byte, error) {
	return rotateVerifiedDBWithSeal(ctx, dbcore.GetDBInstance(), record, token, requestID, expected, protocol, time.Now, seal)
}

func rotateVerifiedDB(ctx context.Context, db *gorm.DB, record models.AgentCredential, token, requestID string, expected *uint64, protocol int, clock func() time.Time) (Issued, error) {
	issued, _, err := rotateVerifiedDBWithSeal(ctx, db, record, token, requestID, expected, protocol, clock, nil)
	return issued, err
}

func unmarshalSealedRefresh(plaintext string) (Issued, []byte, error) {
	var wrapped sealedRefreshRecord
	if err := json.Unmarshal([]byte(plaintext), &wrapped); err == nil && wrapped.Issued.AgentID != "" {
		return wrapped.Issued, wrapped.SignedResponse, nil
	}
	// Backward compatibility for recovery rows sealed before SignedResponse existed.
	var bare Issued
	if err := json.Unmarshal([]byte(plaintext), &bare); err != nil {
		return Issued{}, nil, err
	}
	return bare, nil, nil
}

func rotateVerifiedDBWithSeal(ctx context.Context, db *gorm.DB, record models.AgentCredential, token, requestID string, expected *uint64, protocol int, clock func() time.Time, seal SealRefreshFunc) (Issued, []byte, error) {
	var issued Issued
	var signedResponse []byte
	digest := hashToken(token)
	if digest == "" || record.Client == "" || requestID == "" || (protocol != 1 && protocol != 2) {
		return issued, nil, ErrCredentialRejected
	}
	requestHash := hashToken(requestID)
	for attempt := 0; attempt < 6; attempt++ {
		var outcome error
		issued = Issued{}
		signedResponse = nil
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
			policy, err := loadCredentialPolicy(tx)
			if err != nil {
				return err
			}
			if err = migrateCredential(tx, &cur, policy, now); err != nil {
				return err
			}
			if cur.EnrollmentExpiresAt == nil {
				return errors.New("credential lifecycle deadline missing")
			}
			if !now.Before(*cur.EnrollmentExpiresAt) {
				return fmt.Errorf("%w: enrollment authorization expired; administrator reapproval required", ErrCredentialRejected)
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
					recovered, recoveredSigned, err := unmarshalSealedRefresh(plaintext)
					if err != nil {
						return errors.New("refresh recovery invalid")
					}
					if recovered.AgentID != cur.Client || recovered.Generation != cur.Generation || hashToken(recovered.RefreshToken) != cur.RefreshTokenHash || hashToken(recovered.AccessToken) != cur.AccessTokenHash || !recovered.AccessTokenExpiresAt.Equal(cur.AccessExpiresAt) || !recovered.RefreshTokenExpiresAt.Equal(cur.RefreshExpiresAt) || recovered.RequestID != requestID {
						return errors.New("refresh recovery binding mismatch")
					}
					if expected != nil && *expected+1 != cur.Generation {
						return ErrRefreshConflict
					}
					// Within the 24h recovery window, replay the exact sealed response bytes
					// while its AccessToken is still fresh. If the retry arrives after the 1h
					// AccessToken expired (while RefreshToken and recovery window remain valid),
					// mint a fresh AccessToken for the SAME generation & RefreshToken and
					// re-seal so Agent response validation succeeds across the entire 24h window.
					if !recovered.AccessTokenExpiresAt.After(now.Add(30 * time.Second)) {
						freshAccess := utils.GenerateRandomString(43)
						if freshAccess == "" {
							return errors.New("credential random generation failed")
						}
						recovered.AccessToken = freshAccess
						recovered.AccessTokenExpiresAt = minDeadline(now.Add(AccessTokenLifetime), *cur.EnrollmentExpiresAt)
						var reSigned []byte
						if seal != nil {
							var sealErr error
							reSigned, sealErr = seal(recovered)
							if sealErr != nil {
								return fmt.Errorf("seal recovered refresh response: %w", sealErr)
							}
						}
						plaintext, err := json.Marshal(sealedRefreshRecord{Issued: recovered, SignedResponse: reSigned})
						if err != nil {
							return err
						}
						reSealed, err := secureconfig.EncryptString(string(plaintext))
						if err != nil {
							return errors.New("cannot seal refresh recovery")
						}
						res := tx.Model(&models.AgentCredential{}).Where("client = ? AND generation = ? AND refresh_token_hash = ? AND revoked_at IS NULL", cur.Client, cur.Generation, cur.RefreshTokenHash).Updates(map[string]any{
							"access_token_hash":       hashToken(freshAccess),
							"access_expires_at":       recovered.AccessTokenExpiresAt,
							"refresh_recovery_sealed": reSealed,
							"updated_at":              now,
						})
						if res.Error != nil {
							return res.Error
						}
						if res.RowsAffected != 1 {
							return ErrRefreshConflict
						}
						recoveredSigned = reSigned
					}
					issued = recovered
					signedResponse = recoveredSigned
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
				// Distinguish a late idempotent retry of the same operation (or a
				// pre-upgrade consumed row for the immediately previous generation that
				// predates RequestHash recording) from genuine reuse of a consumed refresh
				// token by a different operation.
				if (consumed.RequestHash != "" && consumed.RequestHash == requestHash) ||
					(consumed.RequestHash == "" && digest == cur.PreviousRefreshHash) {
					return ErrRefreshRecoveryRequired
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
			refreshExp := idleDeadline(cur, policy, now)
			until := minDeadline(now.Add(RefreshRecoveryWindow), refreshExp)
			if protocol == 1 {
				until = minDeadline(now.Add(refreshGracePeriod), refreshExp)
			}
			previousAccessUntil := now.Add(AccessHandoffWindow)
			if cur.AccessExpiresAt.Before(previousAccessUntil) {
				previousAccessUntil = cur.AccessExpiresAt
			}
			issued = Issued{AgentID: cur.Client, AccessToken: access, AccessTokenExpiresAt: minDeadline(now.Add(AccessTokenLifetime), *cur.EnrollmentExpiresAt), RefreshToken: refresh,
				RefreshTokenExpiresAt: refreshExp, Generation: cur.Generation + 1, RequestID: requestID, Scopes: decodeScopes(cur.Scopes)}
			if seal != nil {
				var sealErr error
				signedResponse, sealErr = seal(issued)
				if sealErr != nil {
					return fmt.Errorf("seal signed refresh response: %w", sealErr)
				}
			}
			plaintext, err := json.Marshal(sealedRefreshRecord{Issued: issued, SignedResponse: signedResponse})
			if err != nil {
				return err
			}
			sealed, err := secureconfig.EncryptString(string(plaintext))
			if err != nil {
				return errors.New("cannot seal refresh recovery")
			}
			used := models.AgentRefreshUse{TokenHash: digest, Client: cur.Client, RequestHash: requestHash, Generation: cur.Generation, ConsumedAt: now, ExpiresAt: *cur.EnrollmentExpiresAt}
			if err := tx.Create(&used).Error; err != nil {
				return err
			}
			result := tx.Model(&models.AgentCredential{}).Where("client = ? AND generation = ? AND refresh_token_hash = ? AND revoked_at IS NULL", cur.Client, cur.Generation, digest).Updates(map[string]any{
				"last_refresh_at": now,
				"generation":      issued.Generation, "refresh_protocol": protocol, "access_token_hash": hashToken(access), "access_expires_at": issued.AccessTokenExpiresAt,
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
			return issued, signedResponse, outcome
		}
		if ctx.Err() != nil {
			return Issued{}, nil, ctx.Err()
		}
		// Retry only actual transaction contention, never validation/permission
		// failures. Backoff is bounded and context-cancellable.
		message := strings.ToLower(err.Error())
		retry := strings.Contains(message, "database is locked") || strings.Contains(message, "database table is locked") || strings.Contains(message, "deadlock") || strings.Contains(message, "serialization failure") || strings.Contains(message, "could not serialize")
		if !retry || attempt == 5 {
			return Issued{}, nil, err
		}
		timer := time.NewTimer(time.Duration(attempt+1) * 10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return Issued{}, nil, ctx.Err()
		case <-timer.C:
		}
	}
	return Issued{}, nil, ErrRefreshConflict
}
