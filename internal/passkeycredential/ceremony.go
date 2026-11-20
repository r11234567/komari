package passkeycredential

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/komari-monitor/komari/database/accounts"
	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/models"
	credential "github.com/komari-monitor/komari/internal/trustedcredential"
	"github.com/komari-monitor/komari/pkg/config"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	CeremonyLifetime     = 2 * time.Minute
	loginBudgetWindow    = 5 * time.Minute
	loginIPBudgetLimit   = 30
	loginUserBudgetLimit = 15
	maxLoginPending      = 256
	maxRegisterPending   = 128
	maxPendingPerSource  = 8
	maxBudgetBuckets     = 4096
)

type ceremony struct {
	session              *webauthn.SessionData
	binding              credential.Binding
	name, origin, source string
	synced               bool
	register             bool
}

type ceremonyPool struct {
	mu      sync.Mutex
	maxAll  int
	records map[string]ceremony
}

var (
	loginPool    = &ceremonyPool{maxAll: maxLoginPending, records: make(map[string]ceremony)}
	registerPool = &ceremonyPool{maxAll: maxRegisterPending, records: make(map[string]ceremony)}
)

type rateBucket struct {
	count   int
	expires time.Time
}

var loginLimiter = struct {
	mu      sync.Mutex
	buckets map[string]rateBucket
}{buckets: make(map[string]rateBucket)}

func reserveLoginBudget(ip, username string) bool {
	keys := make([]string, 0, 2)
	limits := make([]int, 0, 2)
	if ip != "" {
		keys = append(keys, "ip:"+ip)
		limits = append(limits, loginIPBudgetLimit)
	}
	if username != "" {
		keys = append(keys, "user:"+strings.ToLower(username))
		limits = append(limits, loginUserBudgetLimit)
	}
	if len(keys) == 0 {
		return true
	}
	now := time.Now()
	loginLimiter.mu.Lock()
	defer loginLimiter.mu.Unlock()
	for k, b := range loginLimiter.buckets {
		if !now.Before(b.expires) {
			delete(loginLimiter.buckets, k)
		}
	}
	missing := 0
	for i, k := range keys {
		b, ok := loginLimiter.buckets[k]
		if ok && b.count >= limits[i] {
			return false
		}
		if !ok {
			missing++
		}
	}
	if len(loginLimiter.buckets)+missing > maxBudgetBuckets {
		return false
	}
	for _, k := range keys {
		b, ok := loginLimiter.buckets[k]
		if !ok {
			b.expires = now.Add(loginBudgetWindow)
		}
		b.count++
		loginLimiter.buckets[k] = b
	}
	return true
}

func poolFor(reg bool) *ceremonyPool {
	if reg {
		return registerPool
	}
	return loginPool
}

func remember(c ceremony, replaceToken string) (string, error) {
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	pool := poolFor(c.register)
	now := time.Now()
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if replaceToken != "" {
		if existing, ok := pool.records[replaceToken]; ok && existing.source == c.source {
			delete(pool.records, replaceToken)
		}
	}
	same := 0
	for k, v := range pool.records {
		if !v.session.Expires.After(now) {
			delete(pool.records, k)
		} else if v.source == c.source {
			same++
		}
	}
	if same >= maxPendingPerSource || len(pool.records) >= pool.maxAll {
		return "", credential.ErrLimit
	}
	c.session.Expires = now.Add(CeremonyLifetime)
	token := base64.RawURLEncoding.EncodeToString(nonce[:])
	pool.records[token] = c
	return token, nil
}

func take(token string, reg bool) (ceremony, error) {
	pool := poolFor(reg)
	pool.mu.Lock()
	defer pool.mu.Unlock()
	c, ok := pool.records[token]
	if !ok || c.register != reg {
		return ceremony{}, credential.ErrVerificationFailed
	}
	delete(pool.records, token)
	if !time.Now().Before(c.session.Expires) {
		return ceremony{}, credential.ErrVerificationFailed
	}
	return c, nil
}

// CancelLogin discards an unused login ceremony belonging to the same source IP.
func CancelLogin(token, ip string) {
	if token == "" {
		return
	}
	loginPool.mu.Lock()
	if existing, ok := loginPool.records[token]; ok && (ip == "" || existing.source == ip) {
		delete(loginPool.records, token)
	}
	loginPool.mu.Unlock()
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
	return BeginLoginWithPrior(ctx, username, ip, "")
}

func BeginLoginWithPrior(ctx context.Context, username, ip, priorToken string) (any, string, error) {
	w, origin, synced, err := nativeVerifier(ctx)
	if err != nil {
		return nil, "", err
	}
	if len(username) > 255 {
		return nil, "", credential.ErrInvalidArgument
	}
	if !reserveLoginBudget(ip, username) {
		return nil, "", credential.ErrLimit
	}
	var options *protocol.CredentialAssertion
	var session *webauthn.SessionData
	if username == "" {
		options, session, err = w.BeginDiscoverableLogin()
	} else {
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
			// Omit optional transports hints on anonymous account-first login so
			// real and synthetic descriptors share the same response shape.
			for i := range a.records {
				a.records[i].Transport = nil
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
	token, err := remember(ceremony{session: session, origin: origin, synced: synced, source: ip}, priorToken)
	return options, token, err
}

func CompleteLogin(ctx context.Context, token string, raw []byte) (string, error) {
	return CompleteLoginWithIP(ctx, "", token, raw)
}

func CompleteLoginWithIP(ctx context.Context, ip, token string, raw []byte) (string, error) {
	if ip != "" && !reserveLoginBudget(ip, "") {
		return "", credential.ErrLimit
	}
	c, err := take(token, false)
	if err != nil {
		return "", err
	}
	if ip == "" && c.source != "" && !reserveLoginBudget(c.source, "") {
		return "", credential.ErrLimit
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
		if !bytes.Equal(c.session.UserID, a.WebAuthnID()) {
			return "", credential.ErrVerificationFailed
		}
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
	var u models.User
	if err := dbcore.GetDBInstance().WithContext(ctx).Select("username").Where("uuid = ?", b.UserID).Take(&u).Error; err == nil {
		a.username = u.Username
	}
	if len(a.records) >= 64 {
		return nil, "", credential.ErrLimit
	}
	options, session, err := w.BeginRegistration(a, webauthn.WithResidentKeyRequirement(protocol.ResidentKeyRequirementRequired), webauthn.WithExclusions(webauthn.Credentials(a.records).CredentialDescriptors()))
	if err != nil {
		return nil, "", err
	}
	token, err := remember(ceremony{session: session, register: true, binding: b, name: name, origin: origin, synced: synced, source: b.UserID}, "")
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
	key, err := w.CreateCredential(account{id: b.UserID, username: user.Username}, *c.session, parsed)
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
		var revoked int64
		if err := tx.Model(&models.PasskeyRevocation{}).Where("credential_hash = ?", row.CredentialHash).Count(&revoked).Error; err != nil {
			return err
		}
		if revoked > 0 {
			return credential.ErrVerificationFailed
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
	disablePasswordLogin, _ := config.GetAs[bool](config.DisablePasswordLoginKey, false)
	oauthEnabled, _ := config.GetAs[bool](config.OAuthEnabledKey, false)
	hasAlternateSignIn := !disablePasswordLogin || (oauthEnabled && strings.TrimSpace(u.SSOID) != "")
	return dbcore.GetDBInstance().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		user := u.UUID
		if err := accounts.RotateCredentialVersion(tx, u); err != nil {
			return err
		}
		var n int64
		q := tx.Model(&models.UserPasskey{}).Where("user_uuid = ? AND rp_id = ? AND credential_hash <> ?", user, w.Config.RPID, k)
		if !synced {
			q = q.Where("device_type = ?", "singleDevice")
		}
		if err := q.Count(&n).Error; err != nil {
			return err
		}
		if n == 0 && ((u.TwoFactor == "" && !hasContact) || !hasAlternateSignIn) {
			return accounts.ErrLastCredential
		}
		res := tx.Where("user_uuid = ? AND rp_id = ? AND credential_hash = ?", user, w.Config.RPID, k).Delete(&models.UserPasskey{})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return credential.ErrNotFound
		}
		revocation := models.PasskeyRevocation{
			CredentialHash: k,
			UserUUID:       user,
			RPID:           w.Config.RPID,
			RevokedAt:      time.Now().UTC(),
		}
		return tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&revocation).Error
	})
}
