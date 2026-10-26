package passkeycredential

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"sync"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/models"
	credential "github.com/komari-monitor/komari/internal/trustedcredential"
	"gorm.io/gorm/clause"
)

type Provider struct {
	// Injectable only inside this Go package for isolated synthetic CI fixtures.
	read func(context.Context, string) (Snapshot, error)
}

func (p Provider) snapshot(ctx context.Context, user string) (Snapshot, error) {
	if p.read != nil {
		return p.read(ctx, user)
	}
	return Read(ctx, user)
}

func (Provider) Descriptor() credential.Descriptor {
	return credential.Descriptor{ID: "plugin:komari-passkey", DisplayName: "Passkey", Kind: "plugin", Purposes: []credential.Purpose{credential.ManageCredentials, credential.UnlockWorkspace, credential.LoginSettings, credential.SensitiveOperation, credential.PasswordLogin}}
}

func (p Provider) Available(ctx context.Context, b credential.Binding) (bool, error) {
	s, err := p.snapshot(ctx, b.UserID)
	if err != nil {
		return false, err
	}
	if !s.Running || len(s.Records) == 0 {
		return false, nil
	}
	_, _, _, err = relyingParty(ctx, s.Config)
	return err == nil, nil
}

type account struct {
	id      string
	records []webauthn.Credential
}

func (a account) WebAuthnID() []byte                         { return []byte(a.id) } // plugin uses UTF-8 UUID, not UUID binary
func (a account) WebAuthnName() string                       { return a.id }
func (a account) WebAuthnDisplayName() string                { return a.id }
func (a account) WebAuthnCredentials() []webauthn.Credential { return a.records }

func (p Provider) Begin(ctx context.Context, attempt credential.Attempt) (credential.Prompt, credential.VerifyFunc, error) {
	s, err := p.snapshot(ctx, attempt.Binding.UserID)
	if err != nil || !s.Running {
		return credential.Prompt{}, nil, credential.ErrProviderUnavailable
	}
	rp, origin, allowSynced, err := relyingParty(ctx, s.Config)
	if err != nil {
		return credential.Prompt{}, nil, err
	}
	user, err := loadAccount(ctx, attempt.Binding.UserID, s.Records, allowSynced)
	if err != nil || len(user.records) == 0 {
		return credential.Prompt{}, nil, credential.ErrProviderUnavailable
	}
	verifier, err := webauthn.New(&webauthn.Config{RPID: rp, RPDisplayName: "Komari", RPOrigins: []string{origin}, AuthenticatorSelection: protocol.AuthenticatorSelection{UserVerification: protocol.VerificationRequired}})
	if err != nil {
		return credential.Prompt{}, nil, credential.ErrProviderUnavailable
	}
	options, session, err := verifier.BeginLogin(user)
	if err != nil {
		return credential.Prompt{}, nil, credential.ErrProviderUnavailable
	}
	session.Expires = attempt.ExpiresAt
	raw, err := json.Marshal(options)
	if err != nil {
		return credential.Prompt{}, nil, credential.ErrProviderUnavailable
	}
	var mu sync.Mutex
	used := false
	return credential.Prompt{Type: "webauthn", Data: raw}, func(ctx context.Context, proof json.RawMessage) error {
		mu.Lock()
		defer mu.Unlock()
		if used || ctx.Err() != nil {
			return credential.ErrVerificationFailed
		}
		current, err := p.snapshot(ctx, attempt.Binding.UserID)
		if err != nil || current.Revision != s.Revision || !current.Running {
			return credential.ErrProviderUnavailable
		}
		_, currentOrigin, _, err := relyingParty(ctx, current.Config)
		if err != nil || currentOrigin != origin {
			return credential.ErrProviderUnavailable
		}
		latest, err := loadAccount(ctx, user.id, current.Records, allowSynced)
		if err != nil {
			return err
		}
		parsed, err := protocol.ParseCredentialRequestResponseBytes(proof)
		if err != nil {
			return credential.ErrVerificationFailed
		}
		verified, err := verifier.ValidateLogin(latest, *session, parsed)
		if err != nil || verified == nil || verified.Authenticator.CloneWarning || !verified.Flags.UserVerified {
			return credential.ErrVerificationFailed
		}
		// One signature can produce at most one result even when SQL fails.
		used = true
		if err := claimCounter(ctx, latest, *verified); err != nil {
			return err
		}
		return nil
	}, nil
}

func keyDigest(id, key []byte) string {
	// JSON separates variable-length fields unambiguously.
	raw, _ := json.Marshal([][]byte{id, key})
	s := sha256.Sum256(raw)
	return hex.EncodeToString(s[:])
}

func loadAccount(ctx context.Context, user string, records []Record, allowSynced bool) (account, error) {
	result := account{id: user}
	for _, record := range records {
		if record.DeviceType == "multiDevice" && !allowSynced {
			continue
		}
		id, _ := base64.RawURLEncoding.DecodeString(record.ID)
		key, _ := base64.RawURLEncoding.DecodeString(record.PublicKey)
		row := models.PasskeyCounter{UserUUID: user, KeyDigest: keyDigest(id, key)}
		db := dbcore.GetDBInstance().WithContext(ctx)
		if err := db.Omit("User").Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error; err != nil {
			return account{}, credential.ErrProviderUnavailable
		}
		if err := db.Where("user_uuid = ? AND key_digest = ?", row.UserUUID, row.KeyDigest).First(&row).Error; err != nil {
			return account{}, credential.ErrProviderUnavailable
		}
		count := max(row.Counter, record.Counter)
		item := webauthn.Credential{ID: id, PublicKey: key, Flags: webauthn.CredentialFlags{UserPresent: true, UserVerified: true, BackupEligible: record.DeviceType == "multiDevice", BackupState: record.BackedUp}, Authenticator: webauthn.Authenticator{SignCount: count}}
		for _, transport := range record.Transports {
			item.Transport = append(item.Transport, protocol.AuthenticatorTransport(transport))
		}
		result.records = append(result.records, item)
	}
	return result, nil
}

func claimCounter(ctx context.Context, user account, verified webauthn.Credential) error {
	digest := keyDigest(verified.ID, verified.PublicKey)
	count := verified.Authenticator.SignCount
	db := dbcore.GetDBInstance().WithContext(ctx)
	// A nonzero authenticator counter must advance the database high-water
	// mark. Zero-counter authenticators still have challenge replay protection.
	query := db.Model(&models.PasskeyCounter{}).Where("user_uuid = ? AND key_digest = ?", user.id, digest)
	if count == 0 {
		var row models.PasskeyCounter
		if query.Where("counter = 0").First(&row).Error != nil {
			return credential.ErrVerificationFailed
		}
		return nil
	}
	result := query.Where("counter < ?", count).Update("counter", count)
	if result.Error != nil {
		return credential.ErrProviderUnavailable
	}
	if result.RowsAffected != 1 {
		return credential.ErrVerificationFailed
	}
	return nil
}
