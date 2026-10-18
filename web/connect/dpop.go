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

var (
	nonceCache sync.Map
)

func recordAndVerifyNonce(nonce string, ttl time.Duration) bool {
	if strings.TrimSpace(nonce) == "" {
		return false
	}
	now := time.Now()
	val, loaded := nonceCache.LoadOrStore(nonce, now.Add(ttl))
	if loaded {
		exp, ok := val.(time.Time)
		if ok && now.Before(exp) {
			return false // replayed
		}
		nonceCache.Store(nonce, now.Add(ttl))
	}
	return true
}

func verifyDPoP(c *gin.Context, principal *rpc.Principal) error {
	sig := c.GetHeader("X-Komari-Signature")
	if sig == "" {
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
	if !recordAndVerifyNonce(nonce, 120*time.Second) {
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

	var bodyBytes []byte
	if c.Request.Body != nil {
		bodyBytes, err = io.ReadAll(c.Request.Body)
		if err != nil {
			return errors.New("dpop: failed to read request body")
		}
		c.Request.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	}
	bodyDigest := sha256.Sum256(bodyBytes)
	bodyHash := hex.EncodeToString(bodyDigest[:])

	pubKey, err := enrollment.ResolveAgentPublicKey(principal.ClientUUID)
	if err != nil {
		return fmt.Errorf("dpop: resolve agent public key: %w", err)
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
