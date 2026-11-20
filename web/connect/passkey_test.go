package connectapi

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/fxamacker/cbor/v2"
	"github.com/google/uuid"
	"github.com/komari-monitor/komari/database/accounts"
	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/internal/credentialgateway"
	"github.com/komari-monitor/komari/internal/passkeycredential"
	"github.com/komari-monitor/komari/pkg/rpc"
	"github.com/komari-monitor/komari/web/security"
	"github.com/pquerna/otp/totp"
	credentialv1 "github.com/r11234567/komari-proto/gen/go/komari/credential/v1"
	"google.golang.org/protobuf/types/known/structpb"
)

func syntheticCOSEKey(t *testing.T) (*ecdsa.PrivateKey, []byte) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := cbor.Marshal(map[int]any{
		1:  2,
		3:  -7,
		-1: 1,
		-2: priv.X.FillBytes(make([]byte, 32)),
		-3: priv.Y.FillBytes(make([]byte, 32)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return priv, pub
}

func connectAttestationStruct(t *testing.T, id, pubCOSE []byte, rpID, origin, challenge string) *structpb.Struct {
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
	authData = append(authData, 0x45) // UP | UV | AT
	authData = binary.BigEndian.AppendUint32(authData, 1)
	authData = append(authData, make([]byte, 16)...)
	authData = binary.BigEndian.AppendUint16(authData, uint16(len(id)))
	authData = append(authData, id...)
	authData = append(authData, pubCOSE...)
	attObj, err := cbor.Marshal(map[string]any{"fmt": "none", "attStmt": map[string]any{}, "authData": authData})
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
			"transports":        []string{"usb"},
		},
		"clientExtensionResults": map[string]any{},
	})
	out := &structpb.Struct{}
	if err := out.UnmarshalJSON(raw); err != nil {
		t.Fatal(err)
	}
	return out
}

func connectAssertionStruct(t *testing.T, priv *ecdsa.PrivateKey, id []byte, owner, rpID, origin, challenge string, counter uint32) *structpb.Struct {
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
	authData = append(authData, 0x05) // UP | UV
	authData = binary.BigEndian.AppendUint32(authData, counter)
	clientHash := sha256.Sum256(client)
	msg := append(append([]byte{}, authData...), clientHash[:]...)
	digest := sha256.Sum256(msg)
	sig, err := ecdsa.SignASN1(rand.Reader, priv, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{
		"id":    enc(id),
		"rawId": enc(id),
		"type":  "public-key",
		"response": map[string]any{
			"clientDataJSON":    enc(client),
			"authenticatorData": enc(authData),
			"signature":         enc(sig),
			"userHandle":        enc([]byte(owner)),
		},
		"clientExtensionResults": map[string]any{},
	})
	out := &structpb.Struct{}
	if err := out.UnmarshalJSON(raw); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestConnectPasskeyLoginAndManagementLifecycle(t *testing.T) {
	t.Setenv("KOMARI_PASSKEY_RP_ID", "connect.example.test")
	t.Setenv("KOMARI_PASSKEY_ORIGIN", "https://connect.example.test")
	t.Setenv("KOMARI_PASSKEY_ALLOW_SYNCED", "false")

	svc := passkeyService{}
	originCtx := passkeycredential.WithRequestOrigin(context.Background(), "connect.example.test", true)

	// 1. GetPasskeyStatus returns enabled=true on matching origin and false on other origins.
	statusRes, err := svc.GetPasskeyStatus(originCtx, connect.NewRequest(&credentialv1.GetPasskeyStatusRequest{}))
	if err != nil || !statusRes.Msg.Enabled {
		t.Fatalf("GetPasskeyStatus on configured origin = (%v, %v)", statusRes, err)
	}
	wrongOriginCtx := passkeycredential.WithRequestOrigin(context.Background(), "other.example.test", true)
	statusOff, err := svc.GetPasskeyStatus(wrongOriginCtx, connect.NewRequest(&credentialv1.GetPasskeyStatusRequest{}))
	if err != nil || statusOff.Msg.Enabled {
		t.Fatalf("GetPasskeyStatus on non-configured origin = (%v, %v)", statusOff, err)
	}

	// 2. Bootstrap register a native passkey with password when user has no factors yet.
	password := "Bootstrap-Password-123"
	user, err := accounts.CreateAccount("pk-connect-"+uuid.NewString(), password)
	if err != nil {
		t.Fatal(err)
	}
	session, err := accounts.CreateSession(user.UUID, 3600, "test-agent", "127.0.0.1", "password")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		credentialgateway.Default.RevokeUser(user.UUID)
		db := dbcore.GetDBInstance()
		db.Where("user_uuid = ?", user.UUID).Delete(&models.UserPasskey{})
		db.Where("user_uuid = ?", user.UUID).Delete(&models.PasskeyCounter{})
		db.Where("user_uuid = ?", user.UUID).Delete(&models.PasskeyRevocation{})
		db.Where("user_uuid = ?", user.UUID).Delete(&models.CredentialContact{})
		db.Where("uuid = ?", user.UUID).Delete(&models.Session{})
		db.Where("uuid = ?", user.UUID).Delete(&models.User{})
	})

	authedCtx := passkeycredential.WithRequestOrigin(
		rpc.NewContextWithMeta(context.Background(), &rpc.ContextMeta{
			Principal:    rpc.NewUserPrincipal(user.UUID),
			UserUUID:     user.UUID,
			SessionToken: session,
			RemoteIP:     "127.0.0.1",
			UserAgent:    "test-agent",
		}),
		"connect.example.test",
		true,
	)

	beginRegRes, err := svc.BeginPasskeyRegistration(authedCtx, connect.NewRequest(&credentialv1.BeginPasskeyRegistrationRequest{
		DisplayName:  "Laptop Key",
		Verification: &credentialv1.BeginPasskeyRegistrationRequest_Password{Password: password},
	}))
	if err != nil || beginRegRes.Msg.ChallengeId == "" {
		t.Fatalf("BeginPasskeyRegistration bootstrap failed: %v", err)
	}
	regPublicKey := beginRegRes.Msg.Options.AsMap()["publicKey"].(map[string]any)
	regChallenge := regPublicKey["challenge"].(string)

	priv, pubCOSE := syntheticCOSEKey(t)
	credID := []byte("connect-passkey-id-" + uuid.NewString())
	att := connectAttestationStruct(t, credID, pubCOSE, "connect.example.test", "https://connect.example.test", regChallenge)
	compRegRes, err := svc.CompletePasskeyRegistration(authedCtx, connect.NewRequest(&credentialv1.CompletePasskeyRegistrationRequest{
		ChallengeId: beginRegRes.Msg.ChallengeId,
		Attestation: att,
	}))
	if err != nil || compRegRes.Msg.Passkey == nil || compRegRes.Msg.Passkey.DisplayName != "Laptop Key" {
		t.Fatalf("CompletePasskeyRegistration failed: %v", err)
	}

	// 3. Direct Passkey Login via Connect RPC (sets __Host-komari_passkey_preauth, retries replace prior cookie, Complete sets session + clears preauth).
	anonCtx := passkeycredential.WithRequestOrigin(
		rpc.NewContextWithMeta(context.Background(), &rpc.ContextMeta{
			Principal: rpc.NewAnonymousPrincipal(),
			RemoteIP:  "198.51.100.77",
			UserAgent: "test-browser",
		}),
		"connect.example.test",
		true,
	)
	firstBegin, err := svc.BeginPasskeyLogin(anonCtx, connect.NewRequest(&credentialv1.BeginPasskeyLoginRequest{}))
	if err != nil {
		t.Fatalf("first BeginPasskeyLogin failed: %v", err)
	}
	firstSetCookie := firstBegin.Header().Get("Set-Cookie")
	if !strings.Contains(firstSetCookie, ceremonyCookie+"=") || !strings.Contains(firstSetCookie, "HttpOnly") || !strings.Contains(firstSetCookie, "Secure") || !strings.Contains(firstSetCookie, "SameSite=Strict") {
		t.Fatalf("unexpected preauth Set-Cookie: %q", firstSetCookie)
	}
	firstCookieVal := strings.SplitN(strings.TrimPrefix(firstSetCookie, ceremonyCookie+"="), ";", 2)[0]

	// Retry passing prior cookie replaces the old ceremony token cleanly.
	retryReq := connect.NewRequest(&credentialv1.BeginPasskeyLoginRequest{Username: user.Username})
	retryReq.Header().Set("Cookie", ceremonyCookie+"="+firstCookieVal)
	secondBegin, err := svc.BeginPasskeyLogin(anonCtx, retryReq)
	if err != nil {
		t.Fatalf("second BeginPasskeyLogin failed: %v", err)
	}
	secondSetCookie := secondBegin.Header().Get("Set-Cookie")
	secondCookieVal := strings.SplitN(strings.TrimPrefix(secondSetCookie, ceremonyCookie+"="), ";", 2)[0]
	loginChallenge := secondBegin.Msg.Options.AsMap()["publicKey"].(map[string]any)["challenge"].(string)

	// Old replaced cookie must no longer verify.
	staleCompReq := connect.NewRequest(&credentialv1.CompletePasskeyLoginRequest{
		Assertion: connectAssertionStruct(t, priv, credID, user.UUID, "connect.example.test", "https://connect.example.test", loginChallenge, 2),
	})
	staleCompReq.Header().Set("Cookie", ceremonyCookie+"="+firstCookieVal)
	if _, err := svc.CompletePasskeyLogin(anonCtx, staleCompReq); err == nil {
		t.Fatal("replaced preauth cookie was still accepted")
	}

	// Current preauth cookie succeeds, issues session cookie, and clears preauth cookie.
	compReq := connect.NewRequest(&credentialv1.CompletePasskeyLoginRequest{
		Assertion: connectAssertionStruct(t, priv, credID, user.UUID, "connect.example.test", "https://connect.example.test", loginChallenge, 2),
	})
	compReq.Header().Set("Cookie", ceremonyCookie+"="+secondCookieVal)
	compRes, err := svc.CompletePasskeyLogin(anonCtx, compReq)
	if err != nil || !compRes.Msg.Authenticated {
		t.Fatalf("CompletePasskeyLogin failed: %v", err)
	}
	clearedPreauth := false
	for _, sc := range compRes.Header().Values("Set-Cookie") {
		if strings.HasPrefix(sc, ceremonyCookie+"=") && strings.Contains(sc, "Max-Age=0") {
			clearedPreauth = true
		}
	}
	if !clearedPreauth {
		t.Fatalf("CompletePasskeyLogin did not clear preauth cookie: %v", compRes.Header().Values("Set-Cookie"))
	}

	// 4. Verify RemoveCredentialContact succeeds when user has an active native Passkey (no false ErrLastCredential).
	reloadedUser, err := accounts.GetUserByUUID(user.UUID)
	if err != nil {
		t.Fatal(err)
	}
	if err := accounts.ChangeVerifiedContact(authedCtx, reloadedUser, models.CredentialContact{
		Kind:      "email",
		Address:   "backup@example.test",
		Transport: "email",
	}); err != nil {
		t.Fatal(err)
	}
	reloadedUser, err = accounts.GetUserByUUID(user.UUID)
	if err != nil {
		t.Fatal(err)
	}
	rpID, allowSynced, ok := passkeycredential.ActivePolicy()
	if !ok {
		t.Fatal("expected ActivePolicy ok")
	}
	deleted, err := accounts.RemoveCredentialContactWithPasskeyPolicy(authedCtx, reloadedUser, "email", rpID, allowSynced)
	if err != nil || !deleted {
		t.Fatalf("RemoveCredentialContactWithPasskeyPolicy failed even though user has native Passkey: deleted=%v err=%v", deleted, err)
	}
}

func TestMisconfiguredPasskeyEnvDoesNotLockOutPasswordWithTOTP(t *testing.T) {
	t.Setenv("KOMARI_PASSKEY_RP_ID", "broken.example.test")
	t.Setenv("KOMARI_PASSKEY_ORIGIN", "http://not-https.example.test")

	password := "Backup-TOTP-Password-123"
	user, err := accounts.CreateAccount("backup-totp-"+uuid.NewString(), password)
	if err != nil {
		t.Fatal(err)
	}
	secret := "JBSWY3DPEHPK3PXP"
	if err := accounts.Enable2Fa(user.UUID, secret); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		credentialgateway.Default.RevokeUser(user.UUID)
		db := dbcore.GetDBInstance()
		db.Where("uuid = ?", user.UUID).Delete(&models.Session{})
		db.Where("uuid = ?", user.UUID).Delete(&models.User{})
	})

	start, err := credentialgateway.Default.BeginPassword(context.Background(), user.Username, password, "198.51.100.88", "test-browser")
	if err != nil {
		t.Fatalf("misconfigured passkey env locked out BeginPassword: %v", err)
	}
	if len(start.Providers) == 0 || start.Providers[0].ID != "native:totp" {
		t.Fatalf("expected native:totp backup provider, got %+v", start.Providers)
	}
	challenge, err := credentialgateway.Default.BeginPasswordVerification(context.Background(), start.Preauthentication, "native:totp")
	if err != nil {
		t.Fatalf("BeginPasswordVerification failed: %v", err)
	}
	code, err := totp.GenerateCode(secret, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	proof, _ := json.Marshal(map[string]string{"code": code})
	sess, uid, err := credentialgateway.Default.CompletePassword(context.Background(), start.Preauthentication, challenge.ID, proof, "198.51.100.88", "test-browser")
	if err != nil || sess == "" || uid != user.UUID {
		t.Fatalf("CompletePassword with TOTP failed under misconfigured passkey env: %v", err)
	}
}

func TestIsRequestSecureTrustsOnlyPrivateProxies(t *testing.T) {
	publicReq := httptest.NewRequest(http.MethodPost, "http://panel.example.test/", nil)
	publicReq.RemoteAddr = "203.0.113.55:44321"
	publicReq.Header.Set("X-Forwarded-Proto", "https")
	if security.IsRequestSecure(publicReq) {
		t.Fatal("spoofed X-Forwarded-Proto from public IP was trusted")
	}

	proxyReq := httptest.NewRequest(http.MethodPost, "http://panel.example.test/", nil)
	proxyReq.RemoteAddr = "127.0.0.1:44321"
	proxyReq.Header.Set("X-Forwarded-Proto", "https, http")
	if !security.IsRequestSecure(proxyReq) {
		t.Fatal("local reverse proxy X-Forwarded-Proto=https was rejected")
	}
}
