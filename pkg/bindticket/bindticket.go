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
	"time"
)

const (
	// TicketDuration defines the 2-minute lifetime for a bind ticket.
	TicketDuration = 2 * time.Minute

	// TicketType identifies tickets meant specifically for /bind enrollment.
	TicketType = "bind_ticket"

	// TicketAudience scopes these tokens to enrollment binding only.
	TicketAudience = "enrollment-bind"

	// TicketScope is the complete capability set available during the ticket's lifetime.
	TicketScope = "enrollment:read enrollment:approve enrollment:deny"
)

var (
	ErrInvalidToken      = errors.New("invalid bind ticket format")
	ErrInvalidSignature  = errors.New("invalid bind ticket signature")
	ErrTicketExpired     = errors.New("bind ticket has expired")
	ErrInvalidTicketType = errors.New("invalid ticket type")
	ErrInvalidAudience   = errors.New("invalid bind ticket audience")
	ErrInvalidScope      = errors.New("invalid bind ticket scope")
	ErrMissingKey        = errors.New("secret key cannot be empty")
)

type Claims struct {
	Sub   string `json:"sub"`
	Type  string `json:"type"`
	Aud   string `json:"aud"`
	Scope string `json:"scope"`
	Jti   string `json:"jti"`
	Iat   int64  `json:"iat"`
	Exp   int64  `json:"exp"`
}

// Issue creates a reusable signed 2-minute ticket for the specified user.
func Issue(userUUID string, key []byte) (string, int64, error) {
	return issueAt(userUUID, key, time.Now().UTC())
}

func issueAt(userUUID string, key []byte, now time.Time) (string, int64, error) {
	if len(key) == 0 {
		return "", 0, ErrMissingKey
	}
	if strings.TrimSpace(userUUID) == "" {
		return "", 0, ErrInvalidToken
	}
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
		Sub:   userUUID,
		Type:  TicketType,
		Aud:   TicketAudience,
		Scope: TicketScope,
		Jti:   jti,
		Iat:   now.Unix(),
		Exp:   exp,
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

// Verify validates all ticket claims. Tickets remain valid until their expiry
// and may authorize multiple enrollment operations during that window.
func Verify(token string, key []byte) (*Claims, error) {
	claims, _, err := parseAndVerifySignature(token, key)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	if claims.Sub == "" || claims.Jti == "" {
		return nil, ErrInvalidToken
	}
	if claims.Iat > now.Add(30*time.Second).Unix() {
		return nil, ErrInvalidToken
	}
	if !now.Before(time.Unix(claims.Exp, 0)) {
		return nil, ErrTicketExpired
	}
	if claims.Type != TicketType {
		return nil, ErrInvalidTicketType
	}
	if claims.Aud != TicketAudience {
		return nil, ErrInvalidAudience
	}
	if claims.Scope != TicketScope {
		return nil, ErrInvalidScope
	}
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

	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, nil, ErrInvalidToken
	}
	var header struct {
		Alg string `json:"alg"`
		Typ string `json:"typ"`
	}
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		return nil, nil, ErrInvalidToken
	}
	if header.Alg != "HS256" || header.Typ != "JWT" {
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
