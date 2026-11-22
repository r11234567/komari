package oauth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/komari-monitor/komari/database/accounts"
)

const (
	bindingTokenTTL        = 5 * time.Minute
	maxPendingBindingCount = 512
)

type bindingRecord struct {
	userUUID      string
	sessionDigest string
	expiresAt     time.Time
}

type bindingStore struct {
	mu      sync.Mutex
	now     func() time.Time
	entries map[[32]byte]bindingRecord
}

var pendingBindings = &bindingStore{
	now:     time.Now,
	entries: make(map[[32]byte]bindingRecord),
}

// IssueBindingToken mints a single-use random binding nonce tied on the server
// to the caller's user UUID and active session digest, eliminating client-chosen
// user UUID cookies in OAuth account binding.
func IssueBindingToken(userUUID, sessionToken string) (string, error) {
	return pendingBindings.issue(userUUID, sessionToken)
}

// ConsumeBindingToken atomically verifies and burns a pending binding nonce
// against the active authenticated user UUID and session cookie.
func ConsumeBindingToken(nonce, userUUID, sessionToken string) bool {
	return pendingBindings.consume(nonce, userUUID, sessionToken)
}

func (s *bindingStore) issue(userUUID, sessionToken string) (string, error) {
	userUUID = strings.TrimSpace(userUUID)
	sessionDigest := accounts.SessionIDForCookie(sessionToken)
	if userUUID == "" || sessionDigest == "" {
		return "", errors.New("valid authenticated session required for OAuth binding")
	}

	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	nonce := hex.EncodeToString(raw[:])
	key := sha256.Sum256([]byte(nonce))

	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entries == nil {
		s.entries = make(map[[32]byte]bindingRecord)
	}
	for k, rec := range s.entries {
		if !now.Before(rec.expiresAt) || rec.sessionDigest == sessionDigest {
			delete(s.entries, k)
		}
	}
	if len(s.entries) >= maxPendingBindingCount {
		return "", errors.New("too many pending OAuth binding requests")
	}
	s.entries[key] = bindingRecord{
		userUUID:      userUUID,
		sessionDigest: sessionDigest,
		expiresAt:     now.Add(bindingTokenTTL),
	}
	return nonce, nil
}

func (s *bindingStore) consume(nonce, userUUID, sessionToken string) bool {
	nonce = strings.TrimSpace(nonce)
	userUUID = strings.TrimSpace(userUUID)
	sessionDigest := accounts.SessionIDForCookie(sessionToken)
	if nonce == "" || userUUID == "" || sessionDigest == "" {
		return false
	}
	key := sha256.Sum256([]byte(nonce))

	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entries == nil {
		return false
	}
	for k, rec := range s.entries {
		if !now.Before(rec.expiresAt) {
			delete(s.entries, k)
		}
	}
	rec, ok := s.entries[key]
	if !ok {
		return false
	}
	delete(s.entries, key)
	if !now.Before(rec.expiresAt) {
		return false
	}
	userMatch := subtle.ConstantTimeCompare([]byte(rec.userUUID), []byte(userUUID)) == 1
	sessionMatch := subtle.ConstantTimeCompare([]byte(rec.sessionDigest), []byte(sessionDigest)) == 1
	return userMatch && sessionMatch
}
