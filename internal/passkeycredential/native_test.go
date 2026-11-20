package passkeycredential

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/fxamacker/cbor/v2"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/google/uuid"
	"github.com/komari-monitor/komari/database/accounts"
	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/models"
	credential "github.com/komari-monitor/komari/internal/trustedcredential"
	"github.com/komari-monitor/komari/pkg/config"
)

func resetNativeTestState() {
	loginPool.mu.Lock()
	loginPool.records = make(map[string]ceremony)
	loginPool.mu.Unlock()
	registerPool.mu.Lock()
	registerPool.records = make(map[string]ceremony)
	registerPool.mu.Unlock()
	loginLimiter.mu.Lock()
	loginLimiter.buckets = make(map[string]rateBucket)
	loginLimiter.mu.Unlock()
}

func signedAssertionFor(t *testing.T, private *ecdsa.PrivateKey, id []byte, owner, rpID, origin, challenge string, counter uint32, flags byte) []byte {
	t.Helper()
	enc := base64.RawURLEncoding.EncodeToString
	client, _ := json.Marshal(map[string]any{
		"type":        "webauthn.get",
		"challenge":   challenge,
		"origin":      origin,
		"crossOrigin": false,
	})
	rpHash := sha256.Sum256([]byte(rpID))
	authData := append([]byte{}, rpHash[:]...)
	authData = append(authData, flags)
	authData = binary.BigEndian.AppendUint32(authData, counter)
	clientHash := sha256.Sum256(client)
	message := append(append([]byte{}, authData...), clientHash[:]...)
	digest := sha256.Sum256(message)
	signature, err := ecdsa.SignASN1(rand.Reader, private, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	proof, _ := json.Marshal(map[string]any{
		"id":    enc(id),
		"rawId": enc(id),
		"type":  "public-key",
		"response": map[string]any{
			"clientDataJSON":    enc(client),
			"authenticatorData": enc(authData),
			"signature":         enc(signature),
			"userHandle":        enc([]byte(owner)),
		},
		"clientExtensionResults": map[string]any{},
	})
	return proof
}

func signedAttestationFor(t *testing.T, id, publicCOSE []byte, rpID, origin, challenge string, counter uint32, flags byte, transports []string) []byte {
	t.Helper()
	enc := base64.RawURLEncoding.EncodeToString
	client, _ := json.Marshal(map[string]any{
		"type":        "webauthn.create",
		"challenge":   challenge,
		"origin":      origin,
		"crossOrigin": false,
	})
	rpHash := sha256.Sum256([]byte(rpID))
	authData := append([]byte{}, rpHash[:]...)
	authData = append(authData, flags|0x40) // attested credential data included
	authData = binary.BigEndian.AppendUint32(authData, counter)
	authData = append(authData, make([]byte, 16)...) // zero AAGUID
	authData = binary.BigEndian.AppendUint16(authData, uint16(len(id)))
	authData = append(authData, id...)
	authData = append(authData, publicCOSE...)
	attObj, err := cbor.Marshal(map[string]any{
		"fmt":      "none",
		"attStmt":  map[string]any{},
		"authData": authData,
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{
		"id":    enc(id),
		"rawId": enc(id),
		"type":  "public-key",
		"response": map[string]any{
			"clientDataJSON":    enc(client),
			"attestationObject": enc(attObj),
			"transports":        transports,
		},
		"clientExtensionResults": map[string]any{},
	})
	return raw
}

func TestNativePolicyMultiOriginPanelSlotAndFailSoftSnapshot(t *testing.T) {
	resetNativeTestState()
	user, err := accounts.CreateAccount("policy-"+uuid.NewString(), "Synthetic-password-123")
	if err != nil {
		t.Fatal(err)
	}
	_, pub := syntheticPublicKey(t)
	enc := base64.RawURLEncoding.EncodeToString
	credID := []byte("policy-cred-" + uuid.NewString())
	if err := dbcore.GetDBInstance().Omit("User").Create(&models.UserPasskey{
		CredentialHash: idDigest(credID),
		CredentialID:   enc(credID),
		UserUUID:       user.UUID,
		RPID:           "example.test",
		Name:           "Primary key",
		PublicKey:      enc(pub),
		DeviceType:     "multiDevice",
	}).Error; err != nil {
		t.Fatal(err)
	}

	// Partial / invalid native env must NOT error out Read() (which would lock out backup TOTP/OTP).
	t.Setenv("KOMARI_PASSKEY_RP_ID", "example.test")
	t.Setenv("KOMARI_PASSKEY_ORIGIN", "http://insecure.example.test")
	t.Setenv("KOMARI_PASSKEY_PANEL_ORIGIN", "")
	t.Setenv("KOMARI_PASSKEY_ALLOW_SYNCED", "")

	snap, err := Read(t.Context(), user.UUID)
	if err != nil {
		t.Fatalf("misconfigured native env must fail soft in Read(), got err: %v", err)
	}
	if snap.Running {
		t.Fatal("misconfigured native snapshot must have Running=false")
	}
	if len(snap.Records) != 1 {
		t.Fatalf("misconfigured native snapshot must preserve enrolled Records to prevent 2FA downgrade, got %d", len(snap.Records))
	}
	ctx := WithRequestOrigin(t.Context(), "panel.example.test", true)
	if AvailableAtOrigin(ctx) {
		t.Fatal("AvailableAtOrigin must be false when env is misconfigured")
	}
	if _, _, err := BeginLogin(ctx, "", "198.51.100.1"); !errors.Is(err, credential.ErrProviderUnavailable) {
		t.Fatalf("expected ErrProviderUnavailable on misconfigured BeginLogin, got %v", err)
	}
	avail, err := (Provider{}).Available(ctx, credential.Binding{UserID: user.UUID})
	if err != nil || avail {
		t.Fatalf("Provider.Available on misconfigured env = (%v, %v), want (false, nil)", avail, err)
	}

	// Multi-origin + P3 panel origin slot + case-insensitive Host matching.
	t.Setenv("KOMARI_PASSKEY_ORIGIN", "https://admin.example.test, https://backup.example.test")
	t.Setenv("KOMARI_PASSKEY_PANEL_ORIGIN", "https://panel.example.test")
	t.Setenv("KOMARI_PASSKEY_ALLOW_SYNCED", "false")

	upperPanelCtx := WithRequestOrigin(t.Context(), "PANEL.EXAMPLE.TEST", true)
	if !AvailableAtOrigin(upperPanelCtx) {
		t.Fatal("AvailableAtOrigin should accept KOMARI_PASSKEY_PANEL_ORIGIN with uppercase host")
	}
	if AvailableAtOrigin(WithRequestOrigin(t.Context(), "evil.example.test", true)) {
		t.Fatal("AvailableAtOrigin accepted unlisted origin")
	}
	// User only has a multiDevice credential while allow_synced=false -> Provider.Available must be false.
	avail, err = (Provider{}).Available(upperPanelCtx, credential.Binding{UserID: user.UUID})
	if err != nil || avail {
		t.Fatalf("Provider.Available must filter multiDevice when allow_synced=false, got (%v, %v)", avail, err)
	}
	t.Setenv("KOMARI_PASSKEY_ALLOW_SYNCED", "true")
	avail, err = (Provider{}).Available(upperPanelCtx, credential.Binding{UserID: user.UUID})
	if err != nil || !avail {
		t.Fatalf("Provider.Available must succeed when allow_synced=true, got (%v, %v)", avail, err)
	}
}

func TestNativeAccountFirstLoginUniformitySecurityAndRateLimits(t *testing.T) {
	resetNativeTestState()
	t.Setenv("KOMARI_PASSKEY_RP_ID", "panel.example.test")
	t.Setenv("KOMARI_PASSKEY_ORIGIN", "https://panel.example.test")
	t.Setenv("KOMARI_PASSKEY_ALLOW_SYNCED", "false")

	ctx := WithRequestOrigin(t.Context(), "panel.example.test", true)
	userA, err := accounts.CreateAccount("acct-a-"+uuid.NewString(), "Synthetic-password-123")
	if err != nil {
		t.Fatal(err)
	}
	userB, err := accounts.CreateAccount("acct-b-"+uuid.NewString(), "Synthetic-password-123")
	if err != nil {
		t.Fatal(err)
	}
	privA, pubA := syntheticPublicKey(t)
	privB, pubB := syntheticPublicKey(t)
	idA := make([]byte, 32)
	idB := make([]byte, 32)
	_, _ = rand.Read(idA)
	_, _ = rand.Read(idB)
	enc := base64.RawURLEncoding.EncodeToString

	db := dbcore.GetDBInstance()
	for _, item := range []struct {
		u   models.User
		id  []byte
		pub []byte
	}{
		{userA, idA, pubA},
		{userB, idB, pubB},
	} {
		if err := db.Omit("User").Create(&models.UserPasskey{
			CredentialHash: idDigest(item.id),
			CredentialID:   enc(item.id),
			UserUUID:       item.u.UUID,
			RPID:           "panel.example.test",
			Name:           "Key",
			PublicKey:      enc(item.pub),
			Transports:     []string{"internal", "hybrid"},
			DeviceType:     "singleDevice",
		}).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Omit("User").Create(&models.PasskeyCounter{
			UserUUID:  item.u.UUID,
			KeyDigest: keyDigest(item.id, item.pub),
			Counter:   0,
		}).Error; err != nil {
			t.Fatal(err)
		}
	}

	// Compare known vs unknown account-first BeginLogin descriptor shape (no transports oracle).
	knownOpt, knownTok, err := BeginLogin(ctx, userA.Username, "203.0.113.10")
	if err != nil {
		t.Fatal(err)
	}
	unknownOpt, unknownTok, err := BeginLogin(ctx, "nonexistent-"+uuid.NewString(), "203.0.113.11")
	if err != nil {
		t.Fatal(err)
	}
	knownReq := knownOpt.(*protocol.CredentialAssertion).Response
	unknownReq := unknownOpt.(*protocol.CredentialAssertion).Response
	if len(knownReq.AllowedCredentials) != 1 || len(unknownReq.AllowedCredentials) != 1 {
		t.Fatalf("expected 1 allowed credential for both known and unknown, got %d vs %d", len(knownReq.AllowedCredentials), len(unknownReq.AllowedCredentials))
	}
	if len(knownReq.AllowedCredentials[0].Transport) != 0 || len(unknownReq.AllowedCredentials[0].Transport) != 0 {
		t.Fatalf("anonymous account-first login must not leak transports: known=%v unknown=%v", knownReq.AllowedCredentials[0].Transport, unknownReq.AllowedCredentials[0].Transport)
	}
	if len(knownReq.AllowedCredentials[0].CredentialID) != len(unknownReq.AllowedCredentials[0].CredentialID) {
		t.Fatalf("unexpected credential ID lengths: known=%d unknown=%d", len(knownReq.AllowedCredentials[0].CredentialID), len(unknownReq.AllowedCredentials[0].CredentialID))
	}

	// Wrong account assertion against knownTok must fail.
	wrongProof := signedAssertionFor(t, privB, idB, userB.UUID, "panel.example.test", "https://panel.example.test", knownReq.Challenge.String(), 1, 0x05)
	if _, err := CompleteLogin(ctx, knownTok, wrongProof); err == nil {
		t.Fatal("wrong-account assertion succeeded")
	}
	// Unknown account ceremony cannot be verified by real credential.
	unknownProof := signedAssertionFor(t, privA, idA, userA.UUID, "panel.example.test", "https://panel.example.test", unknownReq.Challenge.String(), 1, 0x05)
	if _, err := CompleteLogin(ctx, unknownTok, unknownProof); err == nil {
		t.Fatal("unknown-account ceremony accepted real credential")
	}

	// Missing UV flag (0x01 = UP only, no UV) must fail.
	uvOpt, uvTok, err := BeginLogin(ctx, userA.Username, "203.0.113.12")
	if err != nil {
		t.Fatal(err)
	}
	noUVProof := signedAssertionFor(t, privA, idA, userA.UUID, "panel.example.test", "https://panel.example.test", uvOpt.(*protocol.CredentialAssertion).Response.Challenge.String(), 1, 0x01)
	if _, err := CompleteLogin(ctx, uvTok, noUVProof); err == nil {
		t.Fatal("assertion without UV flag succeeded")
	}

	// Policy change mid-ceremony must invalidate pending ceremony.
	polOpt, polTok, err := BeginLogin(ctx, userA.Username, "203.0.113.13")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("KOMARI_PASSKEY_ALLOW_SYNCED", "true")
	polProof := signedAssertionFor(t, privA, idA, userA.UUID, "panel.example.test", "https://panel.example.test", polOpt.(*protocol.CredentialAssertion).Response.Challenge.String(), 1, 0x05)
	if _, err := CompleteLogin(ctx, polTok, polProof); err == nil {
		t.Fatal("policy change did not invalidate pending ceremony")
	}
	t.Setenv("KOMARI_PASSKEY_ALLOW_SYNCED", "false")

	// Zero-counter login succeeds, then incrementing counter succeeds, then concurrent same-ceremony race has only 1 winner.
	zeroOpt, zeroTok, err := BeginLogin(ctx, userA.Username, "203.0.113.14")
	if err != nil {
		t.Fatal(err)
	}
	zeroProof := signedAssertionFor(t, privA, idA, userA.UUID, "panel.example.test", "https://panel.example.test", zeroOpt.(*protocol.CredentialAssertion).Response.Challenge.String(), 0, 0x05)
	if uid, err := CompleteLogin(ctx, zeroTok, zeroProof); err != nil || uid != userA.UUID {
		t.Fatalf("zero-counter account-first login failed: %v", err)
	}

	// Concurrent CompleteLogin race against two parallel ceremonies with the same counter (5): only one CAS wins!
	opt1, tok1, err := BeginLogin(ctx, userA.Username, "203.0.113.15")
	if err != nil {
		t.Fatal(err)
	}
	opt2, tok2, err := BeginLogin(ctx, userA.Username, "203.0.113.16")
	if err != nil {
		t.Fatal(err)
	}
	proof1 := signedAssertionFor(t, privA, idA, userA.UUID, "panel.example.test", "https://panel.example.test", opt1.(*protocol.CredentialAssertion).Response.Challenge.String(), 5, 0x05)
	proof2 := signedAssertionFor(t, privA, idA, userA.UUID, "panel.example.test", "https://panel.example.test", opt2.(*protocol.CredentialAssertion).Response.Challenge.String(), 5, 0x05)

	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, pair := range []struct {
		tok   string
		proof []byte
	}{{tok1, proof1}, {tok2, proof2}} {
		wg.Add(1)
		go func(tok string, proof []byte) {
			defer wg.Done()
			_, err := CompleteLogin(ctx, tok, proof)
			results <- err
		}(pair.tok, pair.proof)
	}
	wg.Wait()
	close(results)
	wins := 0
	for err := range results {
		if err == nil {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("expected exactly 1 counter CAS winner in concurrent login race, got %d", wins)
	}

	// BeginLoginWithPrior replaces prior ceremony so >8 retries from same IP do not hit active-slot limit.
	priorTok := ""
	for i := 0; i < 10; i++ {
		_, tok, err := BeginLoginWithPrior(ctx, "", "203.0.113.20", priorTok)
		if err != nil {
			t.Fatalf("retry %d with prior token replacement failed: %v", i, err)
		}
		priorTok = tok
	}

	// Saturating anonymous login pool must NOT block authenticated BeginRegistration.
	resetNativeTestState()
	for i := 0; i < maxLoginPending; i++ {
		ip := fmt.Sprintf("198.51.100.%d", i/maxPendingPerSource+1)
		if _, _, err := BeginLogin(ctx, "", ip); err != nil {
			t.Fatalf("filling login pool at %d failed: %v", i, err)
		}
	}
	if _, _, err := BeginLogin(ctx, "", "198.51.100.250"); !errors.Is(err, credential.ErrLimit) {
		t.Fatalf("expected login pool full ErrLimit, got %v", err)
	}
	regOpt, regTok, err := BeginRegistration(ctx, credential.Binding{UserID: userA.UUID, SessionDigest: "s", SecurityVersion: "v"}, "Backup Key")
	if err != nil || regTok == "" || regOpt == nil {
		t.Fatalf("registration was starved by full login pool: %v", err)
	}

	// Always-on rate limiting blocks Begin+Complete probe loop from one IP.
	resetNativeTestState()
	hitLimit := false
	for i := 0; i < loginIPBudgetLimit+5; i++ {
		_, tok, err := BeginLogin(ctx, "probe-target", "192.0.2.99")
		if errors.Is(err, credential.ErrLimit) {
			hitLimit = true
			break
		}
		if err != nil {
			t.Fatalf("unexpected error before rate limit at %d: %v", i, err)
		}
		_, _ = CompleteLoginWithIP(ctx, "192.0.2.99", tok, []byte(`{}`))
	}
	if !hitLimit {
		t.Fatal("always-on rate limiter did not block Begin+Complete probing loop")
	}
}

func TestNativeRegistrationDeleteRevocationAndImportConstraints(t *testing.T) {
	resetNativeTestState()
	t.Setenv("KOMARI_PASSKEY_RP_ID", "panel.example.test")
	t.Setenv("KOMARI_PASSKEY_ORIGIN", "https://panel.example.test")
	t.Setenv("KOMARI_PASSKEY_ALLOW_SYNCED", "false")

	ctx := WithRequestOrigin(t.Context(), "panel.example.test", true)
	user, err := accounts.CreateAccount("reg-"+uuid.NewString(), "Synthetic-password-123")
	if err != nil {
		t.Fatal(err)
	}
	binding := credential.Binding{UserID: user.UUID, SessionDigest: "sess-1", SecurityVersion: "ver-1"}

	// Registration exposes username (not raw UUID) in WebAuthn display fields.
	rawOpt, regTok, err := BeginRegistration(ctx, binding, "YubiKey 5")
	if err != nil {
		t.Fatal(err)
	}
	creation := rawOpt.(*protocol.CredentialCreation).Response
	if creation.User.Name != user.Username || creation.User.DisplayName != user.Username {
		t.Fatalf("WebAuthn user display = (%q, %q), want %q", creation.User.Name, creation.User.DisplayName, user.Username)
	}

	_, pub1 := syntheticPublicKey(t)
	credID1 := []byte("reg-cred-1-" + uuid.NewString())
	att1 := signedAttestationFor(t, credID1, pub1, "panel.example.test", "https://panel.example.test", creation.Challenge.String(), 1, 0x05, []string{"usb"})
	row1, err := CompleteRegistration(ctx, binding, user, regTok, att1)
	if err != nil {
		t.Fatalf("CompleteRegistration failed: %v", err)
	}
	if row1.Name != "YubiKey 5" || row1.DeviceType != "singleDevice" {
		t.Fatalf("unexpected registered row: %+v", row1)
	}

	// Reload user because RotateCredentialVersion rotated credential_version.
	user, err = accounts.GetUserByUUID(user.UUID)
	if err != nil {
		t.Fatal(err)
	}

	// Deleting last passkey when password login is disabled and OAuth is off must fail even if TOTP is configured!
	if err := accounts.Enable2Fa(user.UUID, "JBSWY3DPEHPK3PXP"); err != nil {
		t.Fatal(err)
	}
	user, err = accounts.GetUserByUUID(user.UUID)
	if err != nil {
		t.Fatal(err)
	}
	_ = config.Set(config.DisablePasswordLoginKey, true)
	_ = config.Set(config.OAuthEnabledKey, false)
	defer func() {
		_ = config.Set(config.DisablePasswordLoginKey, false)
		_ = config.Set(config.OAuthEnabledKey, false)
	}()
	if err := Delete(ctx, user, row1.CredentialID, true); !errors.Is(err, accounts.ErrLastCredential) {
		t.Fatalf("expected ErrLastCredential when password login disabled and OAuth off, got %v", err)
	}

	// Re-enabling password login allows deleting passkey when TOTP is present, and records revocation tombstone.
	_ = config.Set(config.DisablePasswordLoginKey, false)
	if err := Delete(ctx, user, row1.CredentialID, false); err != nil {
		t.Fatalf("expected Delete to succeed with password+TOTP backup, got %v", err)
	}
	var revCount int64
	if err := dbcore.GetDBInstance().Model(&models.PasskeyRevocation{}).Where("credential_hash = ?", row1.CredentialHash).Count(&revCount).Error; err != nil || revCount != 1 {
		t.Fatalf("expected PasskeyRevocation tombstone, count=%d err=%v", revCount, err)
	}

	// Import must reject previously revoked credential as conflict.
	enc := base64.RawURLEncoding.EncodeToString
	revokedPayload, _ := json.Marshal(map[string]any{
		"version": 1,
		"credentials": []Record{{
			UserUUID:   user.UUID,
			ID:         enc(credID1),
			PublicKey:  enc(pub1),
			DeviceType: "singleDevice",
			Counter:    2,
		}},
	})
	// Clear PasskeyImport marker if previous test set it so we exercise record validation.
	_ = dbcore.GetDBInstance().Where("source = ?", pluginID).Delete(&models.PasskeyImport{}).Error
	report, err := Import(ctx, dbcore.GetDBInstance(), bytes.NewReader(revokedPayload), false)
	if err != nil || report.Conflicts != 1 || report.Eligible != 0 {
		t.Fatalf("expected revoked credential to be flagged as conflict, report=%+v err=%v", report, err)
	}

	// Import enforces per-user 64-passkey cap.
	var bulk []Record
	for i := 0; i < 65; i++ {
		_, pub := syntheticPublicKey(t)
		cid := []byte(fmt.Sprintf("bulk-cap-%d-%s", i, uuid.NewString()))
		bulk = append(bulk, Record{
			UserUUID:   user.UUID,
			ID:         enc(cid),
			PublicKey:  enc(pub),
			DeviceType: "singleDevice",
		})
	}
	bulkPayload, _ := json.Marshal(map[string]any{"version": 1, "credentials": bulk})
	capReport, err := Import(ctx, dbcore.GetDBInstance(), bytes.NewReader(bulkPayload), false)
	if err != nil || capReport.Eligible != 64 || capReport.Conflicts != 1 {
		t.Fatalf("expected 64 eligible + 1 conflict for per-user cap, got %+v err=%v", capReport, err)
	}
}
