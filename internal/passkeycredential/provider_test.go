package passkeycredential

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/google/uuid"
	"github.com/komari-monitor/komari/cmd/flags"
	"github.com/komari-monitor/komari/database/accounts"
	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/models"
	credential "github.com/komari-monitor/komari/internal/trustedcredential"
)

func TestMain(m *testing.M) {
	root, err := os.MkdirTemp("", "komari-passkey-synthetic-*")
	if err != nil {
		panic(err)
	}
	flags.DatabaseType = flags.DatabaseTypeSQLite
	flags.DatabaseFile = filepath.Join(root, "synthetic.db")
	if err := dbcore.Initialize(); err != nil {
		panic(err)
	}
	code := m.Run()
	_ = dbcore.Close()
	_ = os.RemoveAll(root)
	os.Exit(code)
}

func TestIndependentPasskeyVerification(t *testing.T) {
	user, err := accounts.CreateAccount("synthetic-passkey-"+uuid.NewString(), "Synthetic-password-123")
	if err != nil {
		t.Fatal(err)
	}
	private, public := syntheticPublicKey(t)
	enc := base64.RawURLEncoding.EncodeToString
	id := []byte("synthetic-public-credential-id")
	snapshot := Snapshot{Revision: "synthetic-runtime-1", Running: true, Config: map[string]any{}, Records: []Record{{UserUUID: user.UUID, ID: enc(id), PublicKey: enc(public), Counter: 1, DeviceType: "singleDevice"}}}
	p := Provider{read: func(context.Context, string) (Snapshot, error) { return snapshot, nil }}
	ctx := WithRequestOrigin(context.Background(), "panel.example.test", true)
	binding := credential.Binding{UserID: user.UUID, SessionDigest: "synthetic-session", SecurityVersion: "synthetic-version"}
	available, err := p.Available(ctx, binding)
	if err != nil || !available {
		t.Fatal("synthetic provider unavailable", err)
	}
	prompt, verify, err := p.Begin(ctx, credential.Attempt{ChallengeID: "synthetic-challenge", Binding: binding, Request: credential.Request{Purpose: credential.ManageCredentials, Resource: "synthetic-resource"}, ExpiresAt: time.Now().Add(time.Minute)})
	if err != nil || prompt.Type != "webauthn" {
		t.Fatal(err)
	}
	var options struct {
		PublicKey struct {
			Challenge string `json:"challenge"`
			UV        string `json:"userVerification"`
		} `json:"publicKey"`
	}
	if json.Unmarshal(prompt.Data, &options) != nil || options.PublicKey.UV != "required" {
		t.Fatal("user verification must be required")
	}
	proof := signedAssertion(t, private, id, user.UUID, options.PublicKey.Challenge, 2)
	if err := verify(ctx, proof); err != nil {
		t.Fatal("independent valid signature failed", err)
	}
	account, err := loadAccount(ctx, user.UUID, snapshot.Records, false)
	if err != nil || len(account.records) != 1 || account.records[0].Authenticator.SignCount != 2 {
		t.Fatal("trusted high-water mark missing", err)
	}
}

func TestPluginVersionOnePublicRecordContract(t *testing.T) {
	raw := []byte(`{"version":1,"credentials":[{"userUUID":"synthetic-user","id":"aWQ","publicKey":"a2V5","counter":7,"deviceType":"singleDevice","transports":["internal"]},{"userUUID":"other-user","id":"b3RoZXI","publicKey":"a2V5","deviceType":"singleDevice"}],"challenges":[]}`)
	records, err := decodeStore(raw, "synthetic-user")
	if err != nil || len(records) != 1 || records[0].Counter != 7 {
		t.Fatal("plugin record contract changed", err)
	}
	ctx := WithRequestOrigin(context.Background(), "panel.example.test", true)
	rp, origin, synced, err := relyingParty(ctx, map[string]any{})
	if err != nil || rp != "panel.example.test" || origin != "https://panel.example.test" || synced {
		t.Fatal("default relying party policy changed", err)
	}
	rp, origin, synced, err = relyingParty(ctx, map[string]any{"rp_id": "example.test", "origins": "https://other.example.test,\nhttps://panel.example.test", "allow_synced_credentials": true})
	if err != nil || rp != "example.test" || origin != "https://panel.example.test" || !synced {
		t.Fatal("configured relying party policy changed", err)
	}
}

func syntheticPublicKey(t *testing.T) (*ecdsa.PrivateKey, []byte) {
	t.Helper()
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	public, err := cbor.Marshal(map[int]any{1: 2, 3: -7, -1: 1, -2: private.X.FillBytes(make([]byte, 32)), -3: private.Y.FillBytes(make([]byte, 32))})
	if err != nil {
		t.Fatal(err)
	}
	return private, public
}

func signedAssertion(t *testing.T, private *ecdsa.PrivateKey, id []byte, owner, challenge string, counter uint32) []byte {
	t.Helper()
	enc := base64.RawURLEncoding.EncodeToString
	client, _ := json.Marshal(map[string]any{"type": "webauthn.get", "challenge": challenge, "origin": "https://panel.example.test", "crossOrigin": false})
	rpHash := sha256.Sum256([]byte("panel.example.test"))
	authData := append([]byte{}, rpHash[:]...)
	authData = append(authData, 0x05) // user present and verified
	authData = binary.BigEndian.AppendUint32(authData, counter)
	clientHash := sha256.Sum256(client)
	message := append(append([]byte{}, authData...), clientHash[:]...)
	digest := sha256.Sum256(message)
	signature, err := ecdsa.SignASN1(rand.Reader, private, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	proof, _ := json.Marshal(map[string]any{"id": enc(id), "rawId": enc(id), "type": "public-key", "response": map[string]any{"clientDataJSON": enc(client), "authenticatorData": enc(authData), "signature": enc(signature), "userHandle": enc([]byte(owner))}, "clientExtensionResults": map[string]any{}})
	return proof
}

func TestNativeImportAndDiscoverableLogin(t *testing.T) {
	t.Setenv("KOMARI_PASSKEY_RP_ID", "panel.example.test")
	t.Setenv("KOMARI_PASSKEY_ORIGIN", "https://panel.example.test")
	service := credential.New(credential.Options{})
	if err := service.Register(Provider{}); err != nil {
		t.Fatalf("native passkey registration at startup: %v", err)
	}

	ctx := WithRequestOrigin(t.Context(), "panel.example.test", true)
	user, err := accounts.CreateAccount("import-"+uuid.NewString(), "Synthetic-password-123")
	if err != nil {
		t.Fatal(err)
	}
	private, public := syntheticPublicKey(t)
	db := dbcore.GetDBInstance()
	id := []byte(uuid.NewString())
	enc := base64.RawURLEncoding.EncodeToString
	record := Record{ID: enc(id), PublicKey: enc(public), UserUUID: user.UUID, DeviceType: "singleDevice", Counter: 3}
	high := models.PasskeyCounter{UserUUID: user.UUID, KeyDigest: keyDigest(id, public), Counter: 8}
	if err := db.Omit("User").Create(&high).Error; err != nil {
		t.Fatal(err)
	}
	input := func(records ...Record) *bytes.Reader {
		b, err := json.Marshal(map[string]any{"version": 1, "credentials": records})
		if err != nil {
			t.Fatal(err)
		}
		return bytes.NewReader(b)
	}
	unowned := record
	unowned.ID = enc([]byte("unowned"))
	unowned.UserUUID = "unknown"
	if report, err := Import(ctx, db, input(record, record, unowned), false); err != nil || report.Conflicts != 2 || report.Rebind != 1 || report.Eligible != 0 {
		t.Fatalf("reconciliation totals: %+v %v", report, err)
	}
	if _, err := Import(ctx, db, input(record, unowned), true); err == nil {
		t.Fatal("partly valid import committed")
	}
	if n, err := Import(ctx, db, input(record), false); err != nil || n.Eligible != 1 {
		t.Fatalf("preview: %+v %v", n, err)
	}
	keys, err := List(ctx, user.UUID)
	if err != nil || len(keys) != 0 {
		t.Fatalf("rollback/preview persisted records: %v", err)
	}
	if n, err := Import(ctx, db, input(record), true); err != nil || n.Eligible != 1 {
		t.Fatalf("apply: %+v %v", n, err)
	}
	if n, err := Import(ctx, db, input(record), true); err != nil || !n.AlreadyApplied {
		t.Fatalf("retry: %+v %v", n, err)
	}
	a, err := currentAccount(ctx, user.UUID, false)
	if err != nil || len(a.records) != 1 || a.records[0].Authenticator.SignCount != 8 {
		t.Fatalf("lost pre-existing counter: %v", err)
	}
	options, nonce, err := BeginLogin(ctx, "", "local")
	if err != nil {
		t.Fatal(err)
	}
	request := options.(*protocol.CredentialAssertion).Response
	if len(request.AllowedCredentials) != 0 || request.UserVerification != protocol.VerificationRequired {
		t.Fatal("unexpected discoverable options")
	}
	proof := signedAssertion(t, private, id, user.UUID, request.Challenge.String(), 9)
	owner, err := CompleteLogin(ctx, nonce, proof)
	if err != nil || owner != user.UUID {
		t.Fatalf("direct login: %v", err)
	}
	if _, err = CompleteLogin(ctx, nonce, proof); err == nil {
		t.Fatal("completed ceremony was reusable")
	}
	if err = Delete(ctx, user, record.ID, false); err == nil {
		t.Fatal("removed last recovery factor")
	}
}
