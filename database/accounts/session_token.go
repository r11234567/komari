package accounts

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"

	"github.com/komari-monitor/komari/database/dbcore"
	"gorm.io/gorm/clause"
)

// Session cookies are bearer credentials: whoever holds the string is the user
// until it expires. Storing them verbatim means the sessions table, any backup
// of it, and any log or support dump that quotes a row hands over working
// logins. Store a digest instead, so a leaked row cannot be replayed.
//
// The digest is keyed HMAC-SHA256 rather than a bare hash: the tokens are 32
// characters from a known alphabet, so an unkeyed digest of a stolen table is
// worth precomputing against. The key lives in the same database, which does
// not help against a full-database compromise, but does defeat the far more
// common partial leak - a query result, a log line, a table-level backup.
//
// Tokens themselves stay high-entropy random strings; this only changes what
// the server keeps.
const sessionDigestPrefix = "v1:"

// hashSessionToken maps a cookie value to the value stored in the session
// column. Values that already carry the digest prefix are returned unchanged so
// callers can pass either form.
func hashSessionToken(token string) string {
	if token == "" {
		return ""
	}
	if strings.HasPrefix(token, sessionDigestPrefix) {
		return token
	}
	mac := hmac.New(sha256.New, sessionDigestKey())
	mac.Write([]byte(token))
	return sessionDigestPrefix + hex.EncodeToString(mac.Sum(nil))
}

// isSessionDigest reports whether a value is a stored digest rather than a raw
// token. A digest presented as a cookie must be refused: otherwise anyone who
// reads the sessions table could authenticate with its contents directly, which
// is exactly what hashing is meant to prevent.
func isSessionDigest(value string) bool {
	return strings.HasPrefix(value, sessionDigestPrefix)
}

const sessionDigestKeyName = "session_digest_key"

var sessionDigestKeyOnce struct {
	sync.Mutex
	key []byte
}

// sessionDigestKey returns the instance's HMAC key, creating it on first use.
//
// It is derived lazily and kept in the config table rather than a key file, so
// existing deployments need no new operational step and a missing key can never
// stop the server from booting. Losing it only invalidates outstanding cookies,
// which costs everyone a re-login and nothing more - unlike encrypted TOTP
// secrets, session tokens are disposable.
func sessionDigestKey() []byte {
	sessionDigestKeyOnce.Lock()
	defer sessionDigestKeyOnce.Unlock()
	if len(sessionDigestKeyOnce.key) > 0 {
		return sessionDigestKeyOnce.key
	}

	if persisted := loadOrCreateSessionDigestKey(); len(persisted) == 32 {
		sessionDigestKeyOnce.key = persisted
		return sessionDigestKeyOnce.key
	}

	// The database is unavailable or has no usable key. Fall back to a
	// process-lifetime key rather than a fixed one: hashing still holds, and the
	// only cost is that sessions do not survive a restart in this state. A
	// predictable fallback would be worse than not hashing at all, since an
	// attacker could then compute the stored value for a token of their choosing.
	generated := make([]byte, 32)
	if _, err := rand.Read(generated); err != nil {
		panic("accounts: cannot generate session digest key: " + err.Error())
	}
	sessionDigestKeyOnce.key = generated
	return sessionDigestKeyOnce.key
}

// loadOrCreateSessionDigestKey reads the instance key, creating it on first
// use. It returns nil when the database cannot serve one; callers must cope.
func loadOrCreateSessionDigestKey() []byte {
	// Deliberately not GetDBInstance: that exits the process when the database
	// is not ready, which would turn a transient outage - or a unit test that
	// never configures one - into a crash.
	db := dbcore.TryGetDBInstance()
	if db == nil {
		return nil
	}
	read := func() []byte {
		var item struct {
			Value string `gorm:"column:value"`
		}
		if err := db.Table("configs").Where("key = ?", sessionDigestKeyName).First(&item).Error; err != nil {
			return nil
		}
		decoded, err := hex.DecodeString(item.Value)
		if err != nil || len(decoded) != 32 {
			return nil
		}
		return decoded
	}
	if existing := read(); existing != nil {
		return existing
	}

	generated := make([]byte, 32)
	if _, err := rand.Read(generated); err != nil {
		return nil
	}
	// Insert-if-absent so two instances racing on a shared database converge on
	// one key instead of invalidating each other's sessions; re-read to adopt
	// whichever value won.
	if err := db.Table("configs").Clauses(clause.OnConflict{DoNothing: true}).
		Create(map[string]any{"key": sessionDigestKeyName, "value": hex.EncodeToString(generated)}).Error; err != nil {
		return nil
	}
	return read()
}
