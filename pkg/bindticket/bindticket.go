package bindticket

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

const (
	// TicketDuration defines the 2-minute lifetime for a bind ticket.
	TicketDuration = 2 * time.Minute

	// TicketType identifies tickets meant specifically for /bind enrollment.
	TicketType = "bind_ticket"
)

var (
	ErrInvalidToken      = errors.New("invalid bind ticket format")
	ErrInvalidSignature  = errors.New("invalid bind ticket signature")
	ErrTicketExpired     = errors.New("bind ticket has expired")
	ErrInvalidTicketType = errors.New("invalid ticket type")
	ErrTicketAlreadyUsed = errors.New("bind ticket has already been used")
	ErrMissingKey        = errors.New("secret key cannot be empty")
)

type Claims struct {
	Sub  string `json:"sub"`
	Type string `json:"type"`
	Jti  string `json:"jti"`
	Iat  int64  `json:"iat"`
	Exp  int64  `json:"exp"`
}

var (
	redeemedTickets sync.Map // jti -> int64 (expiry unix seconds)
	lastCleanupTime int64
	cleanupMutex    sync.Mutex
)

// Issue creates a new signed 2-minute ticket for the specified user.
func Issue(userUUID string, key []byte) (string, int64, error) {
	if len(key) == 0 {
		return "", 0, ErrMissingKey
	}
	now := time.Now().UTC()
	exp := now.Add(TicketDuration).Unix()

	nonceBytes := make([]byte, 16)
	if _, err := rand.Read(nonceBytes); err != nil {
		return "", 0, fmt.Errorf("failed to generate ticket nonce: %w", err)
	}
	jti := hex.EncodeToString(nonceBytes)

	header := map[string]string{
		"alg": "HS256",
		"typ": "JWT",
	}
	headerJSON, err := json.Marshal(header)
	if err != nil {
		return "", 0, err
	}
	claims := Claims{
		Sub:  userUUID,
		Type: TicketType,
		Jti:  jti,
		Iat:  now.Unix(),
		Exp:  exp,
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return "", 0, err
	}

	headerB64 := base64.RawURLEncoding.EncodeToString(headerJSON)
	claimsB64 := base64.RawURLEncoding.EncodeToString(claimsJSON)
	signingInput := headerB64 + "." + claimsB64

	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(signingInput))
	signatureB64 := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))

	return signingInput + "." + signatureB64, exp, nil
}

// Verify verifies the signature, validity, and expiration of a ticket without consuming it.
func Verify(token string, key []byte) (*Claims, error) {
	claims, _, err := parseAndVerifySignature(token, key)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC().Unix()
	if now > claims.Exp {
		return nil, ErrTicketExpired
	}
	if claims.Type != TicketType {
		return nil, ErrInvalidTicketType
	}
	return claims, nil
}

// VerifyAndConsume validates the ticket and marks it as used (one-time ticket).
func VerifyAndConsume(token string, key []byte) (*Claims, error) {
	claims, err := Verify(token, key)
	if err != nil {
		return nil, err
	}

	// Single-use check: if jti is already in redeemedTickets, reject
	if _, loaded := redeemedTickets.LoadOrStore(claims.Jti, claims.Exp); loaded {
		return nil, ErrTicketAlreadyUsed
	}

	maybePruneRedeemedTickets()
	return claims, nil
}

func parseAndVerifySignature(token string, key []byte) (*Claims, []byte, error) {
	if len(key) == 0 {
		return nil, nil, ErrMissingKey
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, nil, ErrInvalidToken
	}
	signingInput := parts[0] + "." + parts[1]
	providedSig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, nil, ErrInvalidToken
	}

	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(signingInput))
	expectedSig := mac.Sum(nil)

	if !hmac.Equal(providedSig, expectedSig) {
		return nil, nil, ErrInvalidSignature
	}

	claimsJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, nil, ErrInvalidToken
	}
	var claims Claims
	if err := json.Unmarshal(claimsJSON, &claims); err != nil {
		return nil, nil, ErrInvalidToken
	}
	return &claims, claimsJSON, nil
}

func maybePruneRedeemedTickets() {
	now := time.Now().UTC().Unix()
	if now-lastCleanupTime < 60 {
		return
	}
	cleanupMutex.Lock()
	defer cleanupMutex.Unlock()
	if now-lastCleanupTime < 60 {
		return
	}
	lastCleanupTime = now

	redeemedTickets.Range(func(key, value any) bool {
		if exp, ok := value.(int64); ok && now > exp {
			redeemedTickets.Delete(key)
		}
		return true
	})
}

// ResetRedeemedTicketsForTest resets the redeemed tickets tracking in tests.
func ResetRedeemedTicketsForTest() {
	redeemedTickets = sync.Map{}
	lastCleanupTime = 0
}
