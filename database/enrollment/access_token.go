package enrollment

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"strings"
	"sync"
	"time"

	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/utils/secureconfig"
)

const (
	accessTokenPrefix      = "komari-at-"
	accessTokenPayloadSize = 8 + 32 // Unix expiration and random bytes.
)

var accessAdmissionKey = sync.OnceValues(func() ([]byte, error) {
	return secureconfig.DeriveKey("komari/enrollment/access-admission/v1")
})

func mintAccessToken(expires time.Time) (string, error) {
	key, err := accessAdmissionKey()
	if err != nil {
		return "", err
	}
	var data [accessTokenPayloadSize + sha256.Size]byte
	binary.BigEndian.PutUint64(data[:8], uint64(expires.Unix()))
	rand.Read(data[8:accessTokenPayloadSize])
	mac := hmac.New(sha256.New, key)
	mac.Write(data[:accessTokenPayloadSize])
	copy(data[accessTokenPayloadSize:], mac.Sum(nil))
	return accessTokenPrefix + base64.RawURLEncoding.EncodeToString(data[:]), nil
}

// IsIssuedAccessToken only selects the lookup budget. The database remains
// responsible for authentication, handoff deadlines and immediate revocation.
func IsIssuedAccessToken(token string, now time.Time) bool {
	encoded, ok := strings.CutPrefix(token, accessTokenPrefix)
	var data [accessTokenPayloadSize + sha256.Size]byte
	if !ok || len(encoded) != base64.RawURLEncoding.EncodedLen(len(data)) {
		return false
	}
	n, err := base64.RawURLEncoding.Strict().Decode(data[:], []byte(encoded))
	if err != nil || n != len(data) || now.Unix() > int64(binary.BigEndian.Uint64(data[:8])) {
		return false
	}
	key, err := accessAdmissionKey()
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(data[:accessTokenPayloadSize])
	return hmac.Equal(mac.Sum(nil), data[accessTokenPayloadSize:])
}

// StoredAccessTokenDigests seeds admission for opaque tokens issued before
// the current token format. A digest match still requires an authoritative
// lookup, so keeping a hint cannot extend a token's lifetime.
func StoredAccessTokenDigests(ctx context.Context) (map[[sha256.Size]byte]struct{}, error) {
	db := dbcore.TryGetDBInstance()
	if db == nil {
		return nil, ErrCredentialRejected
	}
	rows, err := db.WithContext(ctx).Model(&models.AgentCredential{}).
		Select("access_token_hash", "previous_access_hash").Rows()
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	digests := make(map[[sha256.Size]byte]struct{})
	for rows.Next() {
		var current, previous *string
		if err := rows.Scan(&current, &previous); err != nil {
			return nil, err
		}
		for _, hash := range []*string{current, previous} {
			var digest [sha256.Size]byte
			if hash != nil && len(*hash) == hex.EncodedLen(len(digest)) {
				if _, err := hex.Decode(digest[:], []byte(*hash)); err == nil {
					digests[digest] = struct{}{}
				}
			}
		}
	}
	return digests, rows.Err()
}
