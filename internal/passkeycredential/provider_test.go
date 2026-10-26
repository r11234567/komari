package passkeycredential

import (
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
	"github.com/google/uuid"
	"github.com/komari-monitor/komari/cmd/flags"
	"github.com/komari-monitor/komari/database/accounts"
	"github.com/komari-monitor/komari/database/dbcore"
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
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	public, err := cbor.Marshal(map[int]any{1: 2, 3: -7, -1: 1, -2: private.X.FillBytes(make([]byte, 32)), -3: private.Y.FillBytes(make([]byte, 32))})
	if err != nil {
		t.Fatal(err)
	}
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
	client, _ := json.Marshal(map[string]any{"type": "webauthn.get", "challenge": options.PublicKey.Challenge, "origin": "https://panel.example.test", "crossOrigin": false})
	rpHash := sha256.Sum256([]byte("panel.example.test"))
	authData := append([]byte{}, rpHash[:]...)
	authData = append(authData, 0x05) // user present and verified
	authData = binary.BigEndian.AppendUint32(authData, 2)
	clientHash := sha256.Sum256(client)
	message := append(append([]byte{}, authData...), clientHash[:]...)
	digest := sha256.Sum256(message)
	signature, err := ecdsa.SignASN1(rand.Reader, private, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	proof, _ := json.Marshal(map[string]any{"id": enc(id), "rawId": enc(id), "type": "public-key", "response": map[string]any{"clientDataJSON": enc(client), "authenticatorData": enc(authData), "signature": enc(signature), "userHandle": enc([]byte(user.UUID))}, "clientExtensionResults": map[string]any{}})
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
