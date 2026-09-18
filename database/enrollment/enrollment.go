// Package enrollment stores device authorization attempts and the credentials
// they issue.
//
// Two decisions shape this package.
//
// Tokens are stored hashed. The server only ever needs to check a token it was
// handed, never to reproduce one, so keeping the plaintext would add nothing
// except a database leak that hands over working credentials for every
// machine at once.
//
// An unapproved attempt creates nothing. Rows live in their own table and
// expire, so a machine that was never approved leaves no half-real client
// behind for an operator to discover and wonder about.
package enrollment

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/models"
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
	// RefreshTokenLifetime is the sign-in duration an operator experiences.
	// It is long because re-enrolling requires a human, and forcing that
	// routinely across a fleet is what makes people automate around it.
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
)

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
	RequestedScopes []string
	RemoteIP        string
	Client          string
	ApprovedBy      string
	ApprovedAt      *time.Time
	ExpiresAt       time.Time
	CreatedAt       time.Time
}

// Issued is a freshly minted credential pair. The plaintext tokens exist only
// in this value, on the way to the Agent that asked for them.
type Issued struct {
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
	if len(publicKey) == 0 {
		return Attempt{}, errors.New("an agent public key is required")
	}
	now := time.Now().UTC()
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
	if err := dbcore.GetDBInstance().Create(&row).Error; err != nil {
		return Attempt{}, err
	}
	return toAttempt(row), nil
}

// Poll advances an attempt for the Agent that owns the device code.
//
// It returns the attempt and, when approved, the credentials to hand over. The
// approval itself happens in the panel; this only observes it.
func Poll(deviceCode string) (Attempt, *Issued, error) {
	var attempt Attempt
	var issued *Issued
	err := dbcore.GetDBInstance().Transaction(func(tx *gorm.DB) error {
		row, err := lockAttempt(tx, "device_code = ?", strings.TrimSpace(deviceCode))
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
		issued = &credentials
		return nil
	})
	if errors.Is(err, errSlowDown) {
		return attempt, nil, ErrSlowDown
	}
	if err != nil {
		return Attempt{}, nil, err
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
func Approve(userCode, clientUUID, approvedBy string) (Attempt, error) {
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
			"state":       StateApproved,
			"client":      clientUUID,
			"approved_by": approvedBy,
			"approved_at": now,
			"updated_at":  now,
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

// Refresh rotates a credential pair presented by its refresh token.
//
// The previous token stays valid for a grace period rather than being revoked
// immediately, because the Agent may fail to persist the new pair, and the
// alternative to a grace window is an Agent locked out by a badly timed crash.
func Refresh(refreshToken string) (Issued, models.AgentCredential, error) {
	var issued Issued
	var record models.AgentCredential
	digest := hashToken(refreshToken)
	if digest == "" {
		return issued, record, errors.New("a refresh token is required")
	}
	err := dbcore.GetDBInstance().Transaction(func(tx *gorm.DB) error {
		var row models.AgentCredential
		// The previous hash is accepted so a retry inside the grace window
		// succeeds instead of appearing to be a stolen token.
		if err := tx.Where("refresh_token_hash = ? OR (previous_refresh_hash = ? AND previous_refresh_expires > ?)",
			digest, digest, time.Now().UTC()).First(&row).Error; err != nil {
			return err
		}
		if row.RevokedAt != nil {
			return errors.New("these credentials were revoked")
		}
		now := time.Now().UTC()
		if now.After(row.RefreshExpiresAt) {
			return errors.New("the sign-in has expired; enroll the machine again")
		}
		credentials, err := issueLocked(tx, row.Client, row.AgentPublicKey, row.AgentKeyID, row.KeyAlgorithm, decodeScopes(row.Scopes), now)
		if err != nil {
			return err
		}
		issued = credentials
		record = row
		return nil
	})
	return issued, record, err
}

// Revoke invalidates a machine's credentials immediately.
func Revoke(clientUUID, reason string) error {
	now := time.Now().UTC()
	return dbcore.GetDBInstance().Model(&models.AgentCredential{}).
		Where("client = ?", clientUUID).
		Updates(map[string]any{
			"revoked_at": now, "revoked_reason": trim(reason, 255), "updated_at": now,
		}).Error
}

// ResolveAccessToken identifies the machine presenting an access token.
func ResolveAccessToken(accessToken string) (string, error) {
	digest := hashToken(accessToken)
	if digest == "" {
		return "", errors.New("an access token is required")
	}
	var row models.AgentCredential
	if err := dbcore.GetDBInstance().Where("access_token_hash = ?", digest).First(&row).Error; err != nil {
		return "", err
	}
	if row.RevokedAt != nil {
		return "", errors.New("these credentials were revoked")
	}
	if time.Now().UTC().After(row.AccessExpiresAt) {
		return "", errors.New("the access token has expired")
	}
	return row.Client, nil
}

// GetCredential returns the stored credential record for a machine, which is
// what refresh-proof verification checks a signature against.
func GetCredential(clientUUID string) (models.AgentCredential, error) {
	var row models.AgentCredential
	err := dbcore.GetDBInstance().Where("client = ?", clientUUID).First(&row).Error
	return row, err
}

// issueLocked mints a pair inside an open transaction.
func issueLocked(tx *gorm.DB, clientUUID, publicKey, keyID string, algorithm int32, scopes []string, now time.Time) (Issued, error) {
	accessToken := utils.GenerateRandomString(43)
	refreshToken := utils.GenerateRandomString(43)
	if accessToken == "" || refreshToken == "" {
		return Issued{}, errors.New("failed to generate credentials")
	}
	accessExpiry := now.Add(AccessTokenLifetime)
	refreshExpiry := now.Add(RefreshTokenLifetime)
	graceExpiry := now.Add(refreshGracePeriod)

	var existing models.AgentCredential
	err := tx.Where("client = ?", clientUUID).First(&existing).Error
	switch {
	case err == nil:
		updates := map[string]any{
			"access_token_hash":        hashToken(accessToken),
			"refresh_token_hash":       hashToken(refreshToken),
			"previous_refresh_hash":    existing.RefreshTokenHash,
			"previous_refresh_expires": graceExpiry,
			"access_expires_at":        accessExpiry,
			"refresh_expires_at":       refreshExpiry,
			"agent_public_key":         publicKey,
			"agent_key_id":             keyID,
			"key_algorithm":            algorithm,
			"scopes":                   encodeScopes(scopes),
			// Re-enrolling or refreshing clears a prior revocation: the
			// credential presented was accepted, so continuing to treat the
			// row as revoked would reject a machine that just proved itself.
			"revoked_at":     nil,
			"revoked_reason": "",
			"updated_at":     now,
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
			AccessTokenHash:  hashToken(accessToken),
			RefreshTokenHash: hashToken(refreshToken),
			AccessExpiresAt:  accessExpiry,
			RefreshExpiresAt: refreshExpiry,
			AgentPublicKey:   publicKey,
			AgentKeyID:       keyID,
			KeyAlgorithm:     algorithm,
			Scopes:           encodeScopes(scopes),
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
