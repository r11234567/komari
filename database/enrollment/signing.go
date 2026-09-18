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
	"time"

	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/utils/secureconfig"
	"gorm.io/gorm"
)

// algorithmEd25519 mirrors securityv1.SIGNATURE_ALGORITHM_ED25519 without
// importing the generated package into the database layer.
const algorithmEd25519 int32 = 1

// SigningKey is a usable control-plane key pair.
type SigningKey struct {
	KeyID      string
	Algorithm  int32
	PublicKey  ed25519.PublicKey
	PrivateKey ed25519.PrivateKey
}

// ActiveSigningKey returns the key to sign with, creating one on first use.
//
// Generating on demand means a deployment that never configured signing still
// gets it, rather than silently shipping unsigned instructions because a
// setup step was missed.
func ActiveSigningKey() (SigningKey, error) {
	keys, err := ListControlPlaneKeys()
	if err != nil {
		return SigningKey{}, err
	}
	now := time.Now().UTC()
	for _, key := range keys {
		if key.NotAfter != nil && now.After(*key.NotAfter) {
			continue
		}
		if now.Before(key.NotBefore) {
			continue
		}
		loaded, err := loadPrivateKey(key)
		if err != nil {
			// A public key whose private half is unreadable cannot sign. Skip
			// it rather than failing: another key may be usable, and if none
			// is, a fresh one is generated below.
			continue
		}
		return loaded, nil
	}
	return generateSigningKey(now)
}

// ListControlPlaneKeys returns every published key, newest first.
func ListControlPlaneKeys() ([]models.ControlPlaneKey, error) {
	var rows []models.ControlPlaneKey
	if err := dbcore.GetDBInstance().Order("created_at DESC").Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
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
	now := time.Now().UTC()
	retireAt := now.Add(grace)
	if grace <= 0 {
		retireAt = now
	}
	if err := dbcore.GetDBInstance().Model(&models.ControlPlaneKey{}).
		Where("not_after IS NULL").
		Updates(map[string]any{"not_after": retireAt, "updated_at": now}).Error; err != nil {
		return SigningKey{}, err
	}
	return generateSigningKey(now)
}

func generateSigningKey(now time.Time) (SigningKey, error) {
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
		KeyID:     keyID,
		Algorithm: algorithmEd25519,
		PublicKey: base64.StdEncoding.EncodeToString(public),
		NotBefore: now,
		CreatedAt: now,
	}
	if err := dbcore.GetDBInstance().Create(&row).Error; err != nil {
		return SigningKey{}, err
	}
	if err := storePrivateKey(keyID, sealed); err != nil {
		// Without its private half the published key can never sign, and an
		// Agent that pins it would reject every instruction. Remove it rather
		// than leaving a key that only causes failures.
		_ = dbcore.GetDBInstance().Delete(&models.ControlPlaneKey{}, "key_id = ?", keyID).Error
		return SigningKey{}, err
	}
	return SigningKey{KeyID: keyID, Algorithm: algorithmEd25519, PublicKey: public, PrivateKey: private}, nil
}

// privateKeyRecord holds a sealed private key. It is stored in its own table
// so a routine dump of the published keys cannot include it by accident.
type privateKeyRecord struct {
	KeyID     string    `gorm:"type:varchar(64);primaryKey"`
	Sealed    string    `gorm:"type:text;not null"`
	CreatedAt time.Time `gorm:""`
}

// TableName keeps the table name explicit rather than inferred, since this
// type is deliberately unexported.
func (privateKeyRecord) TableName() string { return "control_plane_private_keys" }

// EnsurePrivateKeyTable creates the private key table.
func EnsurePrivateKeyTable(db *gorm.DB) error {
	return db.AutoMigrate(&privateKeyRecord{})
}

func storePrivateKey(keyID, sealed string) error {
	row := privateKeyRecord{KeyID: keyID, Sealed: sealed, CreatedAt: time.Now().UTC()}
	return dbcore.GetDBInstance().Create(&row).Error
}

func loadPrivateKey(key models.ControlPlaneKey) (SigningKey, error) {
	var row privateKeyRecord
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
	return SigningKey{
		KeyID: key.KeyID, Algorithm: key.Algorithm,
		PublicKey: ed25519.PublicKey(public), PrivateKey: ed25519.PrivateKey(private),
	}, nil
}
