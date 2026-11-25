package enrollment

// Control-plane signing keys.
//
// The panel signs privileged instructions so an Agent can tell a real
// instruction from one injected by whatever sits between them. TLS cannot
// provide that: it commonly terminates at a reverse proxy or access gateway
// that sees plaintext, and anything past that point could forge a message.
//
// Only the public half is stored in the database. The private half lives in
// the secure config store, encrypted at rest, because a database dump that
// included signing keys would let an attacker mint instructions for every
// machine at once.

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/utils/secureconfig"
	enrollmentv1 "github.com/r11234567/komari-proto/gen/go/komari/enrollment/v1"
	securityv1 "github.com/r11234567/komari-proto/gen/go/komari/security/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	"gorm.io/gorm"
)

// algorithmEd25519 mirrors securityv1.SIGNATURE_ALGORITHM_ED25519 without
// importing the generated package into the database layer.
const algorithmEd25519 int32 = 1

var signingKeyLock sync.Mutex

// SigningKey is a usable control-plane key pair.
type SigningKey struct {
	KeyID      string
	Algorithm  int32
	PublicKey  ed25519.PublicKey
	PrivateKey ed25519.PrivateKey
	Revision   uint64
}

// ActiveSigningKey returns the key to sign with, creating one on first use.
//
// Generating on demand means a deployment that never configured signing still
// gets it, rather than silently shipping unsigned instructions because a
// setup step was missed.
func ActiveSigningKey() (SigningKey, error) {
	signingKeyLock.Lock()
	defer signingKeyLock.Unlock()
	return activeSigningKey()
}

func activeSigningKey() (SigningKey, error) {
	keys, err := ListControlPlaneKeys()
	if err != nil {
		return SigningKey{}, err
	}
	now := time.Now().UTC()
	for _, key := range keys {
		if key.NotAfter != nil && !now.Before(*key.NotAfter) {
			continue
		}
		if now.Before(key.NotBefore) {
			continue
		}
		return loadPrivateKey(key)
	}
	if len(keys) != 0 {
		return SigningKey{}, errors.New("no active control plane signing key can be used")
	}
	return createSigningKey(now, SigningKey{}, now)
}

// ListControlPlaneKeys returns every published key, newest first.
func ListControlPlaneKeys() ([]models.ControlPlaneKey, error) {
	var rows []models.ControlPlaneKey
	if err := dbcore.GetDBInstance().Order("trust_revision DESC, created_at DESC, key_id DESC").Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// TrustSigningState snapshots the current signer and its published history.
func TrustSigningState() (SigningKey, []models.ControlPlaneKey, error) {
	signingKeyLock.Lock()
	defer signingKeyLock.Unlock()
	key, err := activeSigningKey()
	if err != nil {
		return SigningKey{}, nil, err
	}
	keys, err := ListControlPlaneKeys()
	return key, keys, err
}

// Sign produces a detached signature over payload with the active key.
func Sign(payload []byte) (keyID string, algorithm int32, signature []byte, err error) {
	key, err := ActiveSigningKey()
	if err != nil {
		return "", 0, nil, err
	}
	return key.KeyID, key.Algorithm, ed25519.Sign(key.PrivateKey, payload), nil
}

// RotateSigningKey publishes a new key and retires the current one after a
// grace period.
//
// The old key keeps verifying during the grace window on purpose: Agents pin
// what they were told, and retiring a key instantly would reject every Agent
// that has not re-fetched the bundle yet.
func RotateSigningKey(grace time.Duration) (SigningKey, error) {
	signingKeyLock.Lock()
	defer signingKeyLock.Unlock()
	previous, err := activeSigningKey()
	if err != nil {
		return SigningKey{}, err
	}
	now := time.Now().UTC()
	return createSigningKey(now, previous, now.Add(max(grace, 0)))
}

func createSigningKey(now time.Time, previous SigningKey, retireAt time.Time) (SigningKey, error) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return SigningKey{}, fmt.Errorf("generate control plane signing key: %w", err)
	}
	digest := sha256.Sum256(public)
	keyID := base64.RawURLEncoding.EncodeToString(digest[:16])

	sealed, err := secureconfig.EncryptString(base64.StdEncoding.EncodeToString(private))
	if err != nil {
		return SigningKey{}, fmt.Errorf("seal control plane signing key: %w", err)
	}

	row := models.ControlPlaneKey{
		KeyID:         keyID,
		Algorithm:     algorithmEd25519,
		PublicKey:     base64.StdEncoding.EncodeToString(public),
		NotBefore:     now,
		CreatedAt:     now,
		TrustRevision: 1,
	}
	if previous.KeyID != "" {
		row.TrustRevision = previous.Revision + 1
		rotation := &enrollmentv1.TrustKeyRotation{
			PreviousKeyId: previous.KeyID, Revision: row.TrustRevision,
			Key: &securityv1.PublicKey{KeyId: keyID, Value: public, Algorithm: securityv1.SignatureAlgorithm_SIGNATURE_ALGORITHM_ED25519, NotBefore: timestamppb.New(now)},
		}
		binding, err := proto.Marshal(rotation)
		if err != nil {
			return SigningKey{}, err
		}
		row.RotationProof, err = proto.Marshal(previous.Envelope(binding))
		if err != nil {
			return SigningKey{}, err
		}
	}
	if err := dbcore.GetDBInstance().Transaction(func(tx *gorm.DB) error {
		if previous.KeyID != "" {
			if err := tx.Model(&models.ControlPlaneKey{}).Where("not_after IS NULL OR not_after > ?", retireAt).
				Updates(map[string]any{"not_after": retireAt, "updated_at": now}).Error; err != nil {
				return err
			}
		}
		if err := tx.Create(&row).Error; err != nil {
			return err
		}
		return tx.Create(&models.ControlPlanePrivateKey{KeyID: keyID, Sealed: sealed, CreatedAt: now}).Error
	}); err != nil {
		return SigningKey{}, err
	}
	return SigningKey{KeyID: keyID, Algorithm: algorithmEd25519, PublicKey: public, PrivateKey: private, Revision: row.TrustRevision}, nil
}

func (key SigningKey) Envelope(payload []byte) *securityv1.SignedEnvelope {
	return &securityv1.SignedEnvelope{Payload: payload, Signatures: []*securityv1.Signature{
		{Algorithm: securityv1.SignatureAlgorithm(key.Algorithm), KeyId: key.KeyID, Value: ed25519.Sign(key.PrivateKey, payload)},
	}}
}

func loadPrivateKey(key models.ControlPlaneKey) (SigningKey, error) {
	var row models.ControlPlanePrivateKey
	if err := dbcore.GetDBInstance().Where("key_id = ?", key.KeyID).First(&row).Error; err != nil {
		return SigningKey{}, err
	}
	plaintext, err := secureconfig.DecryptString(row.Sealed)
	if err != nil {
		return SigningKey{}, fmt.Errorf("unseal control plane signing key: %w", err)
	}
	private, err := base64.StdEncoding.DecodeString(plaintext)
	if err != nil || len(private) != ed25519.PrivateKeySize {
		return SigningKey{}, errors.New("stored control plane signing key is malformed")
	}
	public, err := base64.StdEncoding.DecodeString(key.PublicKey)
	if err != nil || len(public) != ed25519.PublicKeySize {
		return SigningKey{}, errors.New("stored control plane public key is malformed")
	}
	if !ed25519.PublicKey(public).Equal(ed25519.PrivateKey(private).Public()) {
		return SigningKey{}, errors.New("stored control plane key pair does not match")
	}
	return SigningKey{
		KeyID: key.KeyID, Algorithm: key.Algorithm,
		PublicKey: ed25519.PublicKey(public), PrivateKey: ed25519.PrivateKey(private), Revision: max(1, key.TrustRevision),
	}, nil
}
