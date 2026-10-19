package connectapi

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/database/enrollment"
	"github.com/komari-monitor/komari/pkg/rpc"
)

const maxNonceCacheEntries = 65536

var (
	nonceMu    sync.Mutex
	nonceStore = make(map[string]time.Time)
)

func init() {
	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			cleanupExpiredNonces()
		}
	}()
}

func cleanupExpiredNonces() {
	nonceMu.Lock()
	defer nonceMu.Unlock()
	now := time.Now()
	for k, exp := range nonceStore {
		if now.After(exp) {
			delete(nonceStore, k)
		}
	}
}

func recordAndVerifyNonce(nonce string, ttl time.Duration) bool {
	if strings.TrimSpace(nonce) == "" {
		return false
	}
	nonceMu.Lock()
	defer nonceMu.Unlock()

	now := time.Now()
	if exp, exists := nonceStore[nonce]; exists {
		if now.Before(exp) {
			return false // replayed
		}
	}

	if len(nonceStore) >= maxNonceCacheEntries {
		// Prune expired entries immediately
		for k, exp := range nonceStore {
			if now.After(exp) {
				delete(nonceStore, k)
			}
		}
		if len(nonceStore) >= maxNonceCacheEntries {
			// If still full, reject to protect memory
			return false
		}
	}

	nonceStore[nonce] = now.Add(ttl)
	return true
}

func verifyDPoP(c *gin.Context, principal *rpc.Principal) error {
	// Check whether the caller is an enrolled agent with a registered public key.
	var hasPublicKey bool
	var pubKey ed25519.PublicKey
	if principal != nil && principal.Type == rpc.PrincipalAgent && principal.ClientUUID != "" {
		pk, err := enrollment.ResolveAgentPublicKey(principal.ClientUUID)
		if err == nil && len(pk) == ed25519.PublicKeySize {
			hasPublicKey = true
			pubKey = pk
		}
	}

	sig := strings.TrimSpace(c.GetHeader("X-Komari-Signature"))
	if sig == "" {
		// For agents enrolled with Ed25519 public keys, DPoP signatures are mandatory.
		if hasPublicKey {
			return errors.New("dpop: missing required X-Komari-Signature header for enrolled agent")
		}
		// Unauthenticated or legacy tokens without enrolled keys pass through without DPoP.
		return nil
	}

	if principal == nil || principal.Type != rpc.PrincipalAgent || principal.ClientUUID == "" {
		return errors.New("dpop: agent principal required for signed request")
	}

	tsStr := c.GetHeader("X-Komari-Timestamp")
	if tsStr == "" {
		return errors.New("dpop: missing X-Komari-Timestamp header")
	}
	ts, err := strconv.ParseInt(tsStr, 10, 64)
	if err != nil {
		return errors.New("dpop: invalid X-Komari-Timestamp header")
	}
	now := time.Now().Unix()
	if math.Abs(float64(now-ts)) > 120 {
		return errors.New("dpop: timestamp drift exceeds allowed window (120s)")
	}

	nonce := c.GetHeader("X-Komari-Nonce")
	if nonce == "" {
		return errors.New("dpop: missing X-Komari-Nonce header")
	}
	if !recordAndVerifyNonce("dpop:"+principal.ClientUUID+":"+nonce, 120*time.Second) {
		return errors.New("dpop: nonce replay detected")
	}

	authHeader := c.GetHeader("Authorization")
	parts := strings.SplitN(authHeader, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return errors.New("dpop: valid Bearer token required")
	}
	token := parts[1]
	tokenDigest := sha256.Sum256([]byte(token))
	tokenHash := hex.EncodeToString(tokenDigest[:])

	var bodyHash string
	isStream := c.GetHeader("X-Komari-Body-Mode") == "stream" ||
		(strings.HasPrefix(c.GetHeader("Content-Type"), "application/connect+") && c.Request.ContentLength < 0)

	if isStream {
		bodyDigest := sha256.Sum256(nil)
		bodyHash = hex.EncodeToString(bodyDigest[:])
	} else {
		var bodyBytes []byte
		if c.Request.Body != nil {
			bodyBytes, err = io.ReadAll(io.LimitReader(c.Request.Body, 10<<20)) // 10MB limit
			if err != nil {
				return errors.New("dpop: failed to read request body")
			}
			c.Request.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		}
		bodyDigest := sha256.Sum256(bodyBytes)
		bodyHash = hex.EncodeToString(bodyDigest[:])
	}

	if !hasPublicKey {
		pk, err := enrollment.ResolveAgentPublicKey(principal.ClientUUID)
		if err != nil {
			return fmt.Errorf("dpop: resolve agent public key: %w", err)
		}
		pubKey = pk
	}

	// Canonical payload: Method + "\n" + URI_Path + "\n" + Timestamp + "\n" + Nonce + "\n" + SHA256(AccessToken) + "\n" + SHA256(RequestBody)
	payload := fmt.Sprintf("%s\n%s\n%s\n%s\n%s\n%s", c.Request.Method, c.Request.URL.Path, tsStr, nonce, tokenHash, bodyHash)

	sigBytes, err := base64.StdEncoding.DecodeString(sig)
	if err != nil || len(sigBytes) != ed25519.SignatureSize {
		sigBytes, err = hex.DecodeString(sig)
		if err != nil || len(sigBytes) != ed25519.SignatureSize {
			return errors.New("dpop: malformed signature")
		}
	}

	if !ed25519.Verify(pubKey, []byte(payload), sigBytes) {
		return errors.New("dpop: signature verification failed")
	}

	return nil
}
