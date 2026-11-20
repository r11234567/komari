package passkeycredential

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/komari-monitor/komari/database/accounts"
	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/models"
	credential "github.com/komari-monitor/komari/internal/trustedcredential"
	"gorm.io/gorm"
)

const CeremonyLifetime = 2 * time.Minute

type ceremony struct {
	session              *webauthn.SessionData
	binding              credential.Binding
	name, origin, source string
	synced               bool
	register             bool
}

var pending = struct {
	sync.Mutex
	records map[string]ceremony
}{records: make(map[string]ceremony)}

func remember(c ceremony) (string, error) {
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	now := time.Now()
	pending.Lock()
	defer pending.Unlock()
	same := 0
	for k, v := range pending.records {
		if !v.session.Expires.After(now) {
			delete(pending.records, k)
		} else if v.source == c.source {
			same++
		}
	}
	if same >= 8 || len(pending.records) >= 256 {
		return "", credential.ErrLimit
	}
	c.session.Expires = now.Add(CeremonyLifetime)
	token := base64.RawURLEncoding.EncodeToString(nonce[:])
	pending.records[token] = c
	return token, nil
}
func take(token string, reg bool) (ceremony, error) {
	pending.Lock()
	defer pending.Unlock()
	c, ok := pending.records[token]
	if !ok || c.register != reg {
		return ceremony{}, credential.ErrVerificationFailed
	}
	delete(pending.records, token)
	if !time.Now().Before(c.session.Expires) {
		return ceremony{}, credential.ErrVerificationFailed
	}
	return c, nil
}
func AvailableAtOrigin(ctx context.Context) bool {
	_, _, _, err := nativeVerifier(ctx)
	return err == nil
}
func nativeVerifier(ctx context.Context) (*webauthn.WebAuthn, string, bool, error) {
	c, err := nativePolicy()
	if err != nil {
		return nil, "", false, err
	}
	rp, origin, synced, err := relyingParty(ctx, c)
	if err != nil {
		return nil, "", false, err
	}
	w, err := webauthn.New(&webauthn.Config{RPID: rp, RPDisplayName: "Komari", RPOrigins: []string{origin}, AuthenticatorSelection: protocol.AuthenticatorSelection{UserVerification: protocol.VerificationRequired}})
	return w, origin, synced, err
}
func currentAccount(ctx context.Context, id string, synced bool) (account, error) {
	s, err := nativeSnapshot(ctx, id)
	if err != nil {
		return account{}, err
	}
	return loadAccount(ctx, id, s.Records, synced)
}

var accountMaskKey = func() []byte {
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		panic(err)
	}
	return secret[:]
}()

func BeginLogin(ctx context.Context, username, ip string) (any, string, error) {
	w, origin, synced, err := nativeVerifier(ctx)
	if err != nil {
		return nil, "", err
	}
	var options *protocol.CredentialAssertion
	var session *webauthn.SessionData
	if username == "" {
		options, session, err = w.BeginDiscoverableLogin()
	} else {
		if len(username) > 255 {
			return nil, "", credential.ErrInvalidArgument
		}
		var u models.User
		err = dbcore.GetDBInstance().WithContext(ctx).Select("uuid").Where("username = ?", username).Take(&u).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, "", err
		}
		a := account{id: u.UUID}
		if u.UUID != "" {
			a, err = currentAccount(ctx, u.UUID, synced)
			if err != nil {
				return nil, "", err
			}
		}
		if len(a.records) == 0 {
			// Return a normal ceremony for unknown accounts. No stored key will verify it.
			mask := hmac.New(sha256.New, accountMaskKey)
			mask.Write([]byte(username + "\x00" + origin))
			dummy := mask.Sum(nil)
			a = account{id: "unavailable", records: []webauthn.Credential{{ID: dummy}}}
		}
		options, session, err = w.BeginLogin(a)
	}
	if err != nil {
		return nil, "", err
	}
	token, err := remember(ceremony{session: session, origin: origin, synced: synced, source: ip})
	return options, token, err
}
func CompleteLogin(ctx context.Context, token string, raw []byte) (string, error) {
	c, err := take(token, false)
	if err != nil {
		return "", err
	}
	w, origin, synced, err := nativeVerifier(ctx)
	if err != nil || origin != c.origin || c.synced != synced || c.session.RelyingPartyID != w.Config.RPID {
		return "", credential.ErrVerificationFailed
	}
	parsed, err := protocol.ParseCredentialRequestResponseBytes(raw)
	if err != nil {
		return "", credential.ErrVerificationFailed
	}
	id := idDigest(parsed.RawID)
	var row models.UserPasskey
	if err = dbcore.GetDBInstance().WithContext(ctx).Where("credential_hash = ? AND rp_id = ?", id, w.Config.RPID).Take(&row).Error; err != nil {
		return "", credential.ErrVerificationFailed
	}
	a, err := currentAccount(ctx, row.UserUUID, synced)
	if err != nil {
		return "", err
	}
	var verified *webauthn.Credential
	if len(c.session.UserID) == 0 {
		_, verified, err = w.ValidatePasskeyLogin(func(rawID, userHandle []byte) (webauthn.User, error) {
			if !bytes.Equal(userHandle, a.WebAuthnID()) || row.CredentialID != base64.RawURLEncoding.EncodeToString(rawID) {
				return nil, credential.ErrVerificationFailed
			}
			return a, nil
		}, *c.session, parsed)
	} else {
		verified, err = w.ValidateLogin(a, *c.session, parsed)
	}
	if err != nil || verified == nil || verified.Authenticator.CloneWarning || !verified.Flags.UserVerified {
		return "", credential.ErrVerificationFailed
	}
	if err = claimCounter(ctx, a, *verified); err != nil {
		return "", err
	}
	return a.id, nil
}
func BeginRegistration(ctx context.Context, b credential.Binding, name string) (any, string, error) {
	w, origin, synced, err := nativeVerifier(ctx)
	if err != nil {
		return nil, "", err
	}
	a, err := currentAccount(ctx, b.UserID, true)
	if err != nil {
		return nil, "", err
	}
	if len(a.records) >= 64 {
		return nil, "", credential.ErrLimit
	}
	options, session, err := w.BeginRegistration(a, webauthn.WithResidentKeyRequirement(protocol.ResidentKeyRequirementRequired), webauthn.WithExclusions(webauthn.Credentials(a.records).CredentialDescriptors()))
	if err != nil {
		return nil, "", err
	}
	token, err := remember(ceremony{session: session, register: true, binding: b, name: name, origin: origin, synced: synced, source: b.UserID})
	return options, token, err
}
func CompleteRegistration(ctx context.Context, b credential.Binding, user models.User, token string, raw []byte) (models.UserPasskey, error) {
	var row models.UserPasskey
	c, err := take(token, true)
	if err != nil {
		return row, err
	}
	w, origin, synced, err := nativeVerifier(ctx)
	if err != nil || b != c.binding || origin != c.origin || synced != c.synced || w.Config.RPID != c.session.RelyingPartyID {
		return row, credential.ErrVerificationFailed
	}
	parsed, err := protocol.ParseCredentialCreationResponseBytes(raw)
	if err != nil {
		return row, credential.ErrVerificationFailed
	}
	key, err := w.CreateCredential(account{id: b.UserID}, *c.session, parsed)
	if err != nil || !key.Flags.UserVerified || (!synced && key.Flags.BackupEligible) {
		return row, credential.ErrVerificationFailed
	}
	typ := "singleDevice"
	if key.Flags.BackupEligible {
		typ = "multiDevice"
	}
	row = models.UserPasskey{CredentialHash: idDigest(key.ID), CredentialID: base64.RawURLEncoding.EncodeToString(key.ID), UserUUID: b.UserID, RPID: w.Config.RPID, Name: c.name, PublicKey: base64.RawURLEncoding.EncodeToString(key.PublicKey), DeviceType: typ, BackedUp: key.Flags.BackupState}
	for _, v := range key.Transport {
		row.Transports = append(row.Transports, string(v))
	}
	err = dbcore.GetDBInstance().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := accounts.RotateCredentialVersion(tx, user); err != nil {
			return err
		}
		var enrolled int64
		if err := tx.Model(&models.UserPasskey{}).Where("user_uuid = ? AND rp_id = ?", user.UUID, w.Config.RPID).Count(&enrolled).Error; err != nil {
			return err
		}
		if enrolled >= 64 {
			return credential.ErrLimit
		}
		if err := tx.Omit("User").Create(&row).Error; err != nil {
			return err
		}
		return tx.Omit("User").Create(&models.PasskeyCounter{UserUUID: b.UserID, KeyDigest: keyDigest(key.ID, key.PublicKey), Counter: key.Authenticator.SignCount}).Error
	})
	return row, err
}
func List(ctx context.Context, user string) ([]models.UserPasskey, error) {
	w, _, _, err := nativeVerifier(ctx)
	if err != nil {
		return nil, err
	}
	var rows []models.UserPasskey
	err = dbcore.GetDBInstance().WithContext(ctx).Select("credential_id", "name", "created_at", "last_used_at").Where("user_uuid = ? AND rp_id = ?", user, w.Config.RPID).Order("created_at, credential_hash").Find(&rows).Error
	return rows, err
}
func Delete(ctx context.Context, u models.User, id string, hasContact bool) error {
	w, _, synced, err := nativeVerifier(ctx)
	if err != nil {
		return err
	}
	k, err := credentialKey(id)
	if err != nil {
		return credential.ErrInvalidArgument
	}
	return dbcore.GetDBInstance().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		user := u.UUID
		if err := accounts.RotateCredentialVersion(tx, u); err != nil {
			return err
		}
		if u.TwoFactor == "" && !hasContact {
			var n int64
			q := tx.Model(&models.UserPasskey{}).Where("user_uuid = ? AND rp_id = ? AND credential_hash <> ?", user, w.Config.RPID, k)
			if !synced {
				q = q.Where("device_type = ?", "singleDevice")
			}
			if err := q.Count(&n).Error; err != nil {
				return err
			}
			if n == 0 {
				return accounts.ErrLastCredential
			}
		}
		res := tx.Where("user_uuid = ? AND rp_id = ? AND credential_hash = ?", user, w.Config.RPID, k).Delete(&models.UserPasskey{})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return credential.ErrNotFound
		}
		return nil
	})
}
