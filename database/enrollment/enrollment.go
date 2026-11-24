// Package enrollment stores device authorization attempts and the credentials
// they issue.
//
// Two decisions shape this package.
//
// Tokens are stored hashed. Request-bound response recovery uses a short-lived
// encrypted copy protected by the separate panel secret, with no plaintext
// tokens in database columns or audit logs.
//
// An unapproved attempt creates nothing. Rows live in their own table and
// expire, so a machine that was never approved leaves no half-real client
// behind for an operator to discover and wonder about.
package enrollment

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/database/privilegeddelivery"
	"github.com/komari-monitor/komari/utils"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	// deviceCodeLength is long enough that guessing one is not a practical
	// attack, since possession of it is what authorizes the poll.
	deviceCodeLength = 43
	// userCodeLength is short because a human reads or retypes it. The
	// tradeoff is acceptable only because attempts expire quickly and are
	// rate-limited by the poll interval.
	userCodeLength = 8
	// attemptLifetime bounds how long an unapproved attempt stays usable.
	attemptLifetime = 15 * time.Minute
	// AccessTokenLifetime is deliberately short: it is presented on every
	// request, so a captured one should stop working quickly.
	AccessTokenLifetime = 1 * time.Hour
	// RefreshTokenLifetime is the legacy migration horizon. New issuance uses
	// the stored idle policy and a separate fixed enrollment deadline.
	RefreshTokenLifetime = 180 * 24 * time.Hour
	// refreshGracePeriod keeps a superseded refresh token alive briefly, so an
	// Agent that crashes between receiving a rotation and persisting it is not
	// locked out of its own panel.
	refreshGracePeriod = 10 * time.Minute
	// PollInterval is what the server asks Agents to wait between polls.
	PollInterval = 5 * time.Second
	// minimumPollGap is enforced server-side. An Agent polling faster is told
	// to slow down rather than being failed, so a buggy client degrades
	// instead of losing its enrollment.
	minimumPollGap = 2 * time.Second
	// Bound stored anonymous work separately from transient HTTP admission.
	maxEnrollmentAttempts       = 4096
	maxEnrollmentAttemptsPerIP  = 64
	maxEnrollmentAttemptsPerKey = 4
)

var ErrEnrollmentCapacity = errors.New("enrollment request capacity exceeded; wait for pending requests to expire")

var enrollmentAdmissionMu sync.Mutex

// State mirrors the proto enrollment states without importing the generated
// package into the database layer.
const (
	StatePending  int32 = 1
	StateApproved int32 = 3
	StateDenied   int32 = 4
	StateExpired  int32 = 5
)

// Attempt is one device authorization attempt as the panel sees it.
type Attempt struct {
	DeviceCode      string
	UserCode        string
	State           int32
	AgentPublicKey  string
	AgentKeyID      string
	KeyAlgorithm    int32
	Hostname        string
	OperatingSystem string
	Architecture    string
	AgentVersion    string
	HostFingerprint string
	Reenrolled      bool
	RequestedScopes []string
	RemoteIP        string
	Client          string
	ApprovedBy      string
	ApprovedAt      *time.Time
	ExpiresAt       time.Time
	CreatedAt       time.Time
}

// Issued is a freshly minted credential pair. Recovery serializes this value
// only inside an authenticated encryption envelope.
type Issued struct {
	Generation            uint64
	RequestID             string
	AgentID               string
	AccessToken           string
	AccessTokenExpiresAt  time.Time
	RefreshToken          string
	RefreshTokenExpiresAt time.Time
	PreviousTokenExpires  *time.Time
	Scopes                []string
}

// Begin records a new attempt.
func Begin(publicKey []byte, keyID string, algorithm int32, device Attempt) (Attempt, error) {
	return beginDB(dbcore.GetDBInstance(), publicKey, keyID, algorithm, device, time.Now().UTC())
}

func beginDB(db *gorm.DB, publicKey []byte, keyID string, algorithm int32, device Attempt, now time.Time) (Attempt, error) {
	if len(publicKey) == 0 {
		return Attempt{}, errors.New("an agent public key is required")
	}
	row := models.AgentEnrollment{
		DeviceCode:      utils.GenerateRandomString(deviceCodeLength),
		UserCode:        formatUserCode(utils.GenerateRandomString(userCodeLength)),
		State:           StatePending,
		AgentPublicKey:  base64.StdEncoding.EncodeToString(publicKey),
		AgentKeyID:      trim(keyID, 64),
		KeyAlgorithm:    algorithm,
		Hostname:        trim(device.Hostname, 255),
		OperatingSystem: trim(device.OperatingSystem, 64),
		Architecture:    trim(device.Architecture, 32),
		AgentVersion:    trim(device.AgentVersion, 64),
		HostFingerprint: trim(device.HostFingerprint, 128),
		RequestedScopes: encodeScopes(device.RequestedScopes),
		RemoteIP:        trim(device.RemoteIP, 64),
		ExpiresAt:       now.Add(attemptLifetime),
		CreatedAt:       now,
	}
	if row.DeviceCode == "" || row.UserCode == "" {
		return Attempt{}, errors.New("failed to generate enrollment codes")
	}
	enrollmentAdmissionMu.Lock()
	defer enrollmentAdmissionMu.Unlock()
	err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("expires_at <= ?", now).Delete(&models.AgentEnrollment{}).Error; err != nil {
			return err
		}
		for _, limit := range []struct {
			query string
			args  []any
			max   int64
		}{
			{"1 = 1", nil, maxEnrollmentAttempts},
			{"remote_ip = ?", []any{row.RemoteIP}, maxEnrollmentAttemptsPerIP},
			{"agent_public_key = ?", []any{row.AgentPublicKey}, maxEnrollmentAttemptsPerKey},
		} {
			var count int64
			if err := tx.Model(&models.AgentEnrollment{}).Where(limit.query, limit.args...).Count(&count).Error; err != nil {
				return err
			}
			if count >= limit.max {
				return ErrEnrollmentCapacity
			}
		}
		return tx.Create(&row).Error
	}, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return Attempt{}, err
	}
	return toAttempt(row), nil
}

var (
	pollLocks      [64]sync.Mutex
	pollGraceCache sync.Map // map[string]*cachedPollResult
)

type cachedPollResult struct {
	attempt   Attempt
	issued    Issued
	epoch     uint64
	expiresAt time.Time
}

func init() {
	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			now := time.Now().UTC()
			pollGraceCache.Range(func(key, value any) bool {
				if entry, ok := value.(*cachedPollResult); ok {
					if now.After(entry.expiresAt) {
						pollGraceCache.Delete(key)
					}
				}
				return true
			})
		}
	}()
}

// Poll advances an attempt for the Agent that owns the device code.
//
// It returns the attempt and, when approved, the credentials to hand over. The
// approval itself happens in the panel; this only observes it.
func Poll(deviceCode string) (Attempt, *Issued, error) {
	code := strings.TrimSpace(deviceCode)
	if code == "" {
		return Attempt{}, nil, errors.New("a device code is required")
	}

	// The same device code must finish publishing its committed result before a retry reads it.
	lock := &pollLocks[int(sha256.Sum256([]byte(code))[0])%len(pollLocks)]
	lock.Lock()
	defer lock.Unlock()

	// Idempotent grace window: if this device code was recently approved and consumed within 60s,
	// return the same issued credentials to handle network packet loss on the initial response.
	if val, ok := pollGraceCache.Load(code); ok {
		cached := val.(*cachedPollResult)
		if time.Now().UTC().Before(cached.expiresAt) && cached.epoch == dbcore.CurrentSecurityEpoch() {
			issuedCopy := cached.issued
			attempt := cached.attempt
			attempt.Reenrolled = false
			return attempt, &issuedCopy, nil
		}
		pollGraceCache.Delete(code)
	}

	var attempt Attempt
	var issued *Issued
	var cache *cachedPollResult
	err := dbcore.GetDBInstance().Transaction(func(tx *gorm.DB) error {
		row, err := lockAttempt(tx, "device_code = ?", code)
		if err != nil {
			return err
		}
		now := time.Now().UTC()

		// Expiry is evaluated on read rather than by a sweeper, so an attempt
		// cannot be approved in the window between lapsing and being cleaned
		// up.
		if row.State == StatePending && now.After(row.ExpiresAt) {
			row.State = StateExpired
			if err := tx.Model(&row).Updates(map[string]any{"state": StateExpired, "updated_at": now}).Error; err != nil {
				return err
			}
		}

		// Rate limiting reports SLOW_DOWN through the caller rather than
		// failing, so the Agent backs off and keeps its attempt alive.
		tooFast := row.LastPolledAt != nil && now.Sub(*row.LastPolledAt) < minimumPollGap
		if err := tx.Model(&row).Updates(map[string]any{"last_polled_at": now, "updated_at": now}).Error; err != nil {
			return err
		}
		attempt = toAttempt(row)
		if tooFast {
			return errSlowDown
		}
		if row.State != StateApproved || row.Client == "" {
			return nil
		}

		credentials, err := issueLocked(tx, row.Client, row.AgentPublicKey, row.AgentKeyID, row.KeyAlgorithm, decodeScopes(row.RequestedScopes), now)
		if err != nil {
			return err
		}
		if err := tx.Model(&models.AgentCredential{}).Where("client = ?", row.Client).Update("last_approved_by", row.ApprovedBy).Error; err != nil {
			return err
		}
		if !row.CreatedClient {
			if err := privilegeddelivery.ResetForEnrollment(tx, row.Client, now); err != nil {
				return err
			}
			attempt.Reenrolled = true
		}
		issued = &credentials
		cache = &cachedPollResult{
			attempt:   attempt,
			issued:    credentials,
			epoch:     dbcore.CurrentSecurityEpoch(),
			expiresAt: now.Add(60 * time.Second),
		}
		// Atomic single-use invalidation: once credentials have been issued, the attempt row is deleted
		// immediately so the device code cannot be replayed or hijacked after the grace window.
		if err := tx.Delete(&row).Error; err != nil {
			return err
		}
		return nil
	})
	if errors.Is(err, errSlowDown) {
		return attempt, nil, ErrSlowDown
	}
	if err != nil {
		return Attempt{}, nil, err
	}
	if cache != nil {
		pollGraceCache.Store(code, cache)
		if attempt.Reenrolled {
			privilegeddelivery.Notify(attempt.Client)
		}
	}
	return attempt, issued, nil
}

// ErrSlowDown reports that an Agent is polling faster than allowed.
var ErrSlowDown = errors.New("enrollment poll interval is too short")

var errSlowDown = errors.New("slow down")

// GetByUserCode looks up an attempt for the approval page.
func GetByUserCode(userCode string) (Attempt, error) {
	var row models.AgentEnrollment
	normalized := normalizeUserCode(userCode)
	if normalized == "" {
		return Attempt{}, errors.New("a verification code is required")
	}
	if err := dbcore.GetDBInstance().Where("user_code = ?", normalized).First(&row).Error; err != nil {
		return Attempt{}, err
	}
	if row.State == StatePending && time.Now().UTC().After(row.ExpiresAt) {
		row.State = StateExpired
	}
	return toAttempt(row), nil
}

// Approve binds an attempt to a machine.
//
// clientUUID is decided by the caller: the panel either creates a new machine
// or picks an existing one to re-enroll. Doing that here would hide a
// consequential choice inside a storage call.
func Approve(userCode, clientUUID, approvedBy string, createdClient bool) (Attempt, error) {
	var attempt Attempt
	err := dbcore.GetDBInstance().Transaction(func(tx *gorm.DB) error {
		row, err := lockAttempt(tx, "user_code = ?", normalizeUserCode(userCode))
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		if row.State != StatePending {
			return errors.New("this enrollment request is no longer pending")
		}
		if now.After(row.ExpiresAt) {
			_ = tx.Model(&row).Updates(map[string]any{"state": StateExpired, "updated_at": now}).Error
			return errors.New("this enrollment request has expired; start it again on the machine")
		}
		updates := map[string]any{
			"state":          StateApproved,
			"client":         clientUUID,
			"created_client": createdClient,
			"approved_by":    approvedBy,
			"approved_at":    now,
			"updated_at":     now,
		}
		if err := tx.Model(&row).Updates(updates).Error; err != nil {
			return err
		}
		row.State = StateApproved
		row.Client = clientUUID
		row.ApprovedBy = approvedBy
		row.ApprovedAt = &now
		attempt = toAttempt(row)
		return nil
	})
	return attempt, err
}

// Deny rejects an attempt.
func Deny(userCode, deniedBy string) error {
	return dbcore.GetDBInstance().Transaction(func(tx *gorm.DB) error {
		row, err := lockAttempt(tx, "user_code = ?", normalizeUserCode(userCode))
		if err != nil {
			return err
		}
		if row.State != StatePending {
			return errors.New("this enrollment request is no longer pending")
		}
		now := time.Now().UTC()
		return tx.Model(&row).Updates(map[string]any{
			"state": StateDenied, "approved_by": deniedBy, "updated_at": now,
		}).Error
	})
}

// ListPending returns attempts awaiting a decision, newest first.
func ListPending() ([]Attempt, error) {
	var rows []models.AgentEnrollment
	now := time.Now().UTC()
	if err := dbcore.GetDBInstance().
		Where("state = ? AND expires_at > ?", StatePending, now).
		Order("created_at DESC").Find(&rows).Error; err != nil {
		return nil, err
	}
	attempts := make([]Attempt, 0, len(rows))
	for _, row := range rows {
		attempts = append(attempts, toAttempt(row))
	}
	return attempts, nil
}

// ValidateRefreshToken checks if the refresh token is valid and returns the credential record without mutating DB.
func ValidateRefreshToken(refreshToken string) (models.AgentCredential, error) {
	digest := hashToken(refreshToken)
	if digest == "" {
		return models.AgentCredential{}, fmt.Errorf("%w: a refresh token is required", ErrCredentialRejected)
	}
	var row models.AgentCredential
	if err := dbcore.GetDBInstance().Where("refresh_token_hash = ? OR (previous_refresh_hash = ? AND previous_refresh_expires > ?)",
		digest, digest, time.Now().UTC()).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return models.AgentCredential{}, fmt.Errorf("%w: unrecognized refresh credential", ErrCredentialRejected)
		}
		return models.AgentCredential{}, err
	}
	if row.RevokedAt != nil {
		return models.AgentCredential{}, fmt.Errorf("%w: these credentials were revoked", ErrCredentialRejected)
	}
	now := time.Now().UTC()
	if now.After(row.RefreshExpiresAt) {
		return models.AgentCredential{}, fmt.Errorf("%w: the sign-in has expired; enroll the machine again", ErrCredentialRejected)
	}
	return row, nil
}

// Refresh rotates a credential pair presented by its refresh token.
//
// The previous token stays valid for a grace period rather than being revoked
// immediately, because the Agent may fail to persist the new pair, and the
// alternative to a grace window is an Agent locked out by a badly timed crash.
func Refresh(refreshToken string) (Issued, models.AgentCredential, error) {
	record, err := ValidateRefreshToken(refreshToken)
	if err != nil {
		return Issued{}, models.AgentCredential{}, err
	}
	issued, err := RotateVerified(context.Background(), record, refreshToken, "legacy:"+refreshToken, nil, 1)
	return issued, record, err
}

// Revoke invalidates a machine's credentials immediately.
func Revoke(clientUUID, reason string) error {
	now := time.Now().UTC()
	return dbcore.GetDBInstance().Model(&models.AgentCredential{}).
		Where("client = ?", clientUUID).
		Updates(map[string]any{
			"revoked_at": now, "revoked_reason": trim(reason, 255), "updated_at": now,
			"refresh_recovery_sealed": "", "refresh_recovery_until": nil, "refresh_recovery_request": "",
		}).Error
}

// ErrEpochMismatch indicates the panel's security epoch advanced (e.g. after a
// backup restore) and the agent must re-negotiate credentials via signed refresh.
var ErrEpochMismatch = errors.New("credential security epoch requires refresh")

// LookupAccessToken performs a single database lookup for accessToken,
// returning knownRecord=true even when a matching Agent credential is expired
// or revoked so rate-limit lanes classify known Agents without querying twice.
func LookupAccessToken(accessToken string) (clientUUID string, knownRecord bool, err error) {
	return LookupAccessTokenContext(context.Background(), accessToken)
}

func LookupAccessTokenContext(ctx context.Context, accessToken string) (clientUUID string, knownRecord bool, err error) {
	return lookupAccessTokenDB(dbcore.GetDBInstance().WithContext(ctx), accessToken, time.Now().UTC())
}

func lookupAccessTokenDB(db *gorm.DB, accessToken string, now time.Time) (string, bool, error) {
	digest := hashToken(accessToken)
	if digest == "" {
		return "", false, errors.New("an access token is required")
	}
	if db == nil {
		return "", false, errors.New("database unavailable")
	}
	var row models.AgentCredential
	if err := db.Where("access_token_hash = ? OR previous_access_hash = ?", digest, digest).First(&row).Error; err != nil {
		return "", false, err
	}
	// SQLite stores datetime offsets in text. Comparing a legacy local-offset
	// expiry to UTC text can accept an expired handoff. Compare actual instants
	// after decoding, independently of the host timezone or persisted offset.
	if digest != row.AccessTokenHash && (row.PreviousAccessExpires == nil || !now.Before(*row.PreviousAccessExpires)) {
		return "", true, errors.New("the access token handoff has expired")
	}
	if row.RevokedAt != nil {
		return "", true, errors.New("these credentials were revoked")
	}
	if (row.EnrollmentExpiresAt != nil && !now.Before(*row.EnrollmentExpiresAt)) || (row.LifecycleVersion > 0 && !now.Before(row.RefreshExpiresAt)) {
		return "", true, errors.New("machine authorization expired; administrator reapproval required")
	}
	if !now.Before(row.AccessExpiresAt) {
		return "", true, errors.New("the access token has expired")
	}
	if curEpoch := dbcore.CurrentSecurityEpoch(); curEpoch > 1 && row.SecurityEpoch < curEpoch {
		return "", true, ErrEpochMismatch
	}
	return row.Client, true, nil
}

// ResolveAccessToken identifies the machine presenting an access token.
func ResolveAccessToken(accessToken string) (string, error) {
	clientUUID, _, err := LookupAccessToken(accessToken)
	return clientUUID, err
}

// RescueWatchEnrollmentKey identifies the already authenticated enrollment that
// opened a rescue stream, even when its access token is subsequently rotated.
func RescueWatchEnrollmentKey(ctx context.Context, agentID, token string) (string, error) {
	var row models.AgentCredential
	digest := hashToken(token)
	if digest == "" {
		return "", errors.New("rescue watch requires an access token")
	}
	err := dbcore.GetDBInstance().WithContext(ctx).Select("agent_key_id").
		First(&row, "client = ? AND (access_token_hash = ? OR previous_access_hash = ?)", agentID, digest, digest).Error
	return row.AgentKeyID, err
}

func RescueWatchEnrollmentWithdrawn(ctx context.Context, agentID, key string) (bool, error) {
	var row models.AgentCredential
	err := dbcore.GetDBInstance().WithContext(ctx).
		Select("agent_key_id", "revoked_at", "enrollment_expires_at", "lifecycle_version", "refresh_expires_at").
		First(&row, "client = ?", agentID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	now := time.Now().UTC()
	return row.AgentKeyID != key || row.RevokedAt != nil ||
		row.EnrollmentExpiresAt != nil && !now.Before(*row.EnrollmentExpiresAt) ||
		row.LifecycleVersion > 0 && !now.Before(row.RefreshExpiresAt), nil
}

// HasAccessTokenRecord reports whether accessToken matches a stored (possibly
// expired or revoked) Agent access token digest so NAT rate-limit lanes can
// separate legitimate expired Agents from PATs and unknown brute-force guesses.
func HasAccessTokenRecord(accessToken string) bool {
	return hasAccessTokenRecordDB(dbcore.GetDBInstance(), accessToken)
}

func hasAccessTokenRecordDB(db *gorm.DB, accessToken string) bool {
	_, known, _ := lookupAccessTokenDB(db, accessToken, time.Now().UTC())
	return known
}

// GetCredential returns the stored credential record for a machine, which is
// what refresh-proof verification checks a signature against.
func GetCredential(clientUUID string) (models.AgentCredential, error) {
	var row models.AgentCredential
	err := dbcore.GetDBInstance().Where("client = ?", clientUUID).First(&row).Error
	return row, err
}

// ResolveAgentPublicKey decodes and returns the registered Ed25519 public key for an agent.
func ResolveAgentPublicKey(clientUUID string) (ed25519.PublicKey, error) {
	key, _, err := ResolveAgentProofKey(clientUUID)
	return key, err
}

// ResolveAgentProofKey returns the pinned key and whether it has upgraded to JWT proofs.
func ResolveAgentProofKey(clientUUID string) (ed25519.PublicKey, bool, error) {
	cred, err := GetCredential(clientUUID)
	if err != nil {
		return nil, false, err
	}
	raw, err := base64.StdEncoding.DecodeString(cred.AgentPublicKey)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, false, errors.New("invalid agent public key")
	}
	return ed25519.PublicKey(raw), cred.DPoPJWTRequired, nil
}

// RequireAgentDPoPJWT records the upgrade for the key that proved possession.
func RequireAgentDPoPJWT(ctx context.Context, agentID string, key ed25519.PublicKey) error {
	return dbcore.GetDBInstance().WithContext(ctx).Model(&models.AgentCredential{}).
		Where("client = ? AND agent_public_key = ? AND revoked_at IS NULL", agentID, base64.StdEncoding.EncodeToString(key)).
		UpdateColumn("dpop_jwt_required", true).Error
}

// issueLocked mints a pair inside an open transaction.
func issueLocked(tx *gorm.DB, clientUUID, publicKey, keyID string, algorithm int32, scopes []string, now time.Time) (Issued, error) {
	refreshToken := utils.GenerateRandomString(43)
	if refreshToken == "" {
		return Issued{}, errors.New("failed to generate credentials")
	}
	policy, err := loadCredentialPolicy(tx)
	if err != nil {
		return Issued{}, err
	}
	absoluteExpiry := now.Add(time.Duration(policy.AbsoluteDays) * credentialDay)
	accessExpiry := minDeadline(now.Add(AccessTokenLifetime), absoluteExpiry)
	accessToken, err := mintAccessToken(accessExpiry)
	if err != nil {
		return Issued{}, fmt.Errorf("mint access token: %w", err)
	}
	refreshExpiry := minDeadline(now.Add(time.Duration(policy.IdleDays)*credentialDay), absoluteExpiry)
	graceExpiry := now.Add(refreshGracePeriod)

	var existing models.AgentCredential
	err = tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("client = ?", clientUUID).First(&existing).Error
	switch {
	case err == nil:
		if existing.RevokedAt != nil && existing.AgentPublicKey == publicKey {
			return Issued{}, errors.New("cannot issue credentials for a revoked agent")
		}
		updates := map[string]any{
			"generation":       existing.Generation + 1,
			"refresh_protocol": 0, "revoked_at": nil, "revoked_reason": "",
			"lifecycle_version": 1, "enrollment_expires_at": absoluteExpiry, "last_refresh_at": now, "migration_grace_until": nil, "last_approved_at": now,
			"refresh_recovery_sealed": "", "refresh_recovery_request": "", "refresh_recovery_until": nil,
			"previous_access_hash": "", "previous_access_expires": nil,
			"access_token_hash":        hashToken(accessToken),
			"refresh_token_hash":       hashToken(refreshToken),
			"previous_refresh_hash":    "",
			"previous_refresh_expires": graceExpiry,
			"access_expires_at":        accessExpiry,
			"refresh_expires_at":       refreshExpiry,
			"agent_public_key":         publicKey,
			"agent_key_id":             keyID,
			"key_algorithm":            algorithm,
			"scopes":                   encodeScopes(scopes),
			"security_epoch":           dbcore.CurrentSecurityEpoch(),
			"updated_at":               now,
		}
		if existing.AgentPublicKey != publicKey {
			updates["dpop_jwt_required"] = false
		}
		if err := tx.Where("client = ?", clientUUID).Delete(&models.AgentRefreshUse{}).Error; err != nil {
			return Issued{}, err
		}
		if err := tx.Model(&existing).Updates(updates).Error; err != nil {
			return Issued{}, err
		}
		return Issued{
			AgentID: clientUUID, AccessToken: accessToken, AccessTokenExpiresAt: accessExpiry,
			RefreshToken: refreshToken, RefreshTokenExpiresAt: refreshExpiry,
			PreviousTokenExpires: &graceExpiry, Scopes: scopes,
		}, nil
	case errors.Is(err, gorm.ErrRecordNotFound):
		row := models.AgentCredential{
			Client:           clientUUID,
			LifecycleVersion: 1, EnrollmentExpiresAt: &absoluteExpiry, LastRefreshAt: &now, LastApprovedAt: &now,
			AccessTokenHash:  hashToken(accessToken),
			RefreshTokenHash: hashToken(refreshToken),
			AccessExpiresAt:  accessExpiry,
			RefreshExpiresAt: refreshExpiry,
			AgentPublicKey:   publicKey,
			AgentKeyID:       keyID,
			KeyAlgorithm:     algorithm,
			Scopes:           encodeScopes(scopes),
			SecurityEpoch:    dbcore.CurrentSecurityEpoch(),
			CreatedAt:        now,
		}
		if err := tx.Create(&row).Error; err != nil {
			return Issued{}, err
		}
		return Issued{
			AgentID: clientUUID, AccessToken: accessToken, AccessTokenExpiresAt: accessExpiry,
			RefreshToken: refreshToken, RefreshTokenExpiresAt: refreshExpiry, Scopes: scopes,
		}, nil
	default:
		return Issued{}, err
	}
}

func lockAttempt(tx *gorm.DB, query string, args ...any) (models.AgentEnrollment, error) {
	var row models.AgentEnrollment
	// Locking keeps two concurrent approvals, or an approval racing a poll,
	// from both deciding they won.
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where(query, args...).First(&row).Error
	return row, err
}

func toAttempt(row models.AgentEnrollment) Attempt {
	return Attempt{
		DeviceCode: row.DeviceCode, UserCode: row.UserCode, State: row.State,
		AgentPublicKey: row.AgentPublicKey, AgentKeyID: row.AgentKeyID, KeyAlgorithm: row.KeyAlgorithm,
		Hostname: row.Hostname, OperatingSystem: row.OperatingSystem, Architecture: row.Architecture,
		AgentVersion: row.AgentVersion, HostFingerprint: row.HostFingerprint,
		RequestedScopes: decodeScopes(row.RequestedScopes), RemoteIP: row.RemoteIP,
		Client: row.Client, ApprovedBy: row.ApprovedBy, ApprovedAt: row.ApprovedAt,
		ExpiresAt: row.ExpiresAt, CreatedAt: row.CreatedAt,
	}
}

// PublicKeyBytes decodes the stored agent key for signature verification.
func (attempt Attempt) PublicKeyBytes() ([]byte, error) {
	return base64.StdEncoding.DecodeString(attempt.AgentPublicKey)
}

// hashToken renders the digest stored in place of a plaintext token.
func hashToken(token string) string {
	trimmed := strings.TrimSpace(token)
	if trimmed == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(trimmed))
	return hex.EncodeToString(digest[:])
}

// formatUserCode groups the code so a human reading it aloud does not lose
// their place.
func formatUserCode(value string) string {
	upper := strings.ToUpper(value)
	if len(upper) != userCodeLength {
		return upper
	}
	return upper[:4] + "-" + upper[4:]
}

func normalizeUserCode(value string) string {
	return strings.ToUpper(strings.TrimSpace(value))
}

func encodeScopes(scopes []string) string {
	if len(scopes) == 0 {
		return ""
	}
	data, err := json.Marshal(scopes)
	if err != nil {
		return ""
	}
	return string(data)
}

func decodeScopes(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	var scopes []string
	if err := json.Unmarshal([]byte(value), &scopes); err != nil {
		return nil
	}
	return scopes
}

func trim(value string, limit int) string {
	trimmed := strings.TrimSpace(value)
	if len(trimmed) <= limit {
		return trimmed
	}
	return trimmed[:limit]
}

// Candidate is an existing machine an attempt may be a re-enrollment of.
type Candidate struct {
	ClientUUID       string
	Name             string
	MatchReason      string
	FingerprintMatch bool
}

// Candidates suggests existing machines for an attempt, strongest first.
//
// A host fingerprint recorded by an earlier approved enrollment is strong
// evidence, since it derives from the machine ID rather than anything the
// operator named. A matching name is weak and is offered only as a hint: two
// hosts can share a hostname, and the administrator decides either way.
func Candidates(attempt Attempt) ([]Candidate, error) {
	db := dbcore.GetDBInstance()
	seen := make(map[string]bool)
	var result []Candidate

	if attempt.HostFingerprint != "" {
		var previous []models.AgentEnrollment
		if err := db.Where("host_fingerprint = ? AND state = ? AND client <> ''",
			attempt.HostFingerprint, StateApproved).
			Order("approved_at DESC").Limit(5).Find(&previous).Error; err != nil {
			return nil, err
		}
		for _, row := range previous {
			if seen[row.Client] {
				continue
			}
			var client models.Client
			if err := db.Where("uuid = ?", row.Client).First(&client).Error; err != nil {
				continue
			}
			seen[row.Client] = true
			result = append(result, Candidate{
				ClientUUID: client.UUID, Name: client.Name,
				MatchReason: "same host fingerprint as an earlier enrollment", FingerprintMatch: true,
			})
		}
	}

	if hostname := strings.TrimSpace(attempt.Hostname); hostname != "" {
		var byName []models.Client
		if err := db.Where("name = ?", hostname).Limit(5).Find(&byName).Error; err != nil {
			return nil, err
		}
		for _, client := range byName {
			if seen[client.UUID] {
				continue
			}
			seen[client.UUID] = true
			result = append(result, Candidate{
				ClientUUID: client.UUID, Name: client.Name,
				MatchReason: "machine name matches the reported hostname",
			})
		}
	}
	return result, nil
}

// KeyFingerprint renders the agent key the way the Agent's CLI prints it, so an
// administrator can compare the two by eye.
func (attempt Attempt) KeyFingerprint() string {
	raw, err := attempt.PublicKeyBytes()
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(raw)
	return base64.RawStdEncoding.EncodeToString(digest[:])
}
