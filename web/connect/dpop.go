package connectapi

import (
	"bytes"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/database/enrollment"
	"github.com/komari-monitor/komari/pkg/requestmeta"
	"github.com/komari-monitor/komari/pkg/rpc"
	"gorm.io/gorm"
)

const (
	maxDPoPNonceEntries    = 65536
	maxDPoPPerSubject      = 1024
	maxRefreshNonceEntries = 16384
	maxRefreshPerSubject   = 256
	maxNonceLength         = 128
	maxDPoPBodyBytes       = 10 << 20
	maxDPoPJWTBytes        = 8192
	dpopNonceBucketSeconds = 60
)

// replayEntry tracks an active replay-prevention key and its owner bucket.
type replayEntry struct {
	subject string
	expires time.Time
}

// replayCache is a bounded, per-subject partitioned in-memory replay guard.
// Separate caches isolate high-rate DPoP traffic from credential refresh proofs.
type replayCache struct {
	mu            sync.Mutex
	maxTotal      int
	maxPerSubject int
	entries       map[string]replayEntry
	perSubject    map[string]int
}

func newReplayCache(maxTotal, maxPerSubject int) *replayCache {
	return &replayCache{
		maxTotal:      maxTotal,
		maxPerSubject: maxPerSubject,
		entries:       make(map[string]replayEntry),
		perSubject:    make(map[string]int),
	}
}

func (r *replayCache) pruneLocked(now time.Time) {
	for k, e := range r.entries {
		if !now.Before(e.expires) {
			delete(r.entries, k)
			if r.perSubject[e.subject] <= 1 {
				delete(r.perSubject, e.subject)
			} else {
				r.perSubject[e.subject]--
			}
		}
	}
}

func (r *replayCache) CheckAndRecord(subject, nonce string, ttl time.Duration) bool {
	subject = strings.TrimSpace(subject)
	nonce = strings.TrimSpace(nonce)
	if subject == "" || nonce == "" || len(nonce) > maxNonceLength {
		return false
	}
	key := subject + "\x00" + nonce

	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()
	if existing, exists := r.entries[key]; exists {
		if now.Before(existing.expires) {
			return false
		}
		delete(r.entries, key)
		if r.perSubject[existing.subject] <= 1 {
			delete(r.perSubject, existing.subject)
		} else {
			r.perSubject[existing.subject]--
		}
	}

	if r.perSubject[subject] >= r.maxPerSubject || len(r.entries) >= r.maxTotal {
		r.pruneLocked(now)
		if r.perSubject[subject] >= r.maxPerSubject || len(r.entries) >= r.maxTotal {
			return false
		}
	}

	r.entries[key] = replayEntry{subject: subject, expires: now.Add(ttl)}
	r.perSubject[subject]++
	return true
}

func (r *replayCache) Cleanup() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneLocked(time.Now())
}

var (
	dpopReplayCache    = newReplayCache(maxDPoPNonceEntries, maxDPoPPerSubject)
	refreshReplayCache = newReplayCache(maxRefreshNonceEntries, maxRefreshPerSubject)

	dpopNonceKeyOnce sync.Once
	dpopNonceKey     [32]byte
)

func init() {
	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			dpopReplayCache.Cleanup()
			refreshReplayCache.Cleanup()
		}
	}()
}

func dpopHMACSecret() []byte {
	dpopNonceKeyOnce.Do(func() {
		if _, err := io.ReadFull(rand.Reader, dpopNonceKey[:]); err != nil {
			panic(fmt.Sprintf("dpop: generate hmac key: %v", err))
		}
	})
	return dpopNonceKey[:]
}

func dpopNonceForBucket(agentID string, bucket int64) string {
	mac := hmac.New(sha256.New, dpopHMACSecret())
	_, _ = mac.Write([]byte(agentID))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(strconv.FormatInt(bucket, 10)))
	return strconv.FormatInt(bucket, 10) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func currentDPoPNonce(agentID string, now time.Time) string {
	return dpopNonceForBucket(agentID, now.Unix()/dpopNonceBucketSeconds)
}

// AdvertiseAgentProof handles the unauthenticated compatibility probe. A
// challenge is scoped to the supplied identifier and says nothing about
// whether that identifier is registered.
func AdvertiseAgentProof(c *gin.Context) {
	c.Writer.Header().Add("Vary", "X-Komari-Agent-Nonce-For")
	agentID := c.GetHeader("X-Komari-Agent-Nonce-For")
	if agentID == "" || len(agentID) > 128 ||
		strings.IndexFunc(agentID, func(r rune) bool {
			return r != '-' && r != '_' && !(r >= '0' && r <= '9') &&
				!(r >= 'A' && r <= 'Z') && !(r >= 'a' && r <= 'z')
		}) >= 0 {
		return
	}
	c.Header("Cache-Control", "no-store")
	c.Header("X-Komari-Agent-Proof", "dpop-jwt-v1")
	c.Header("DPoP-Nonce", currentDPoPNonce(agentID, time.Now()))
}

func verifyDPoPServerNonce(agentID, raw string, now time.Time) bool {
	bucketStr, macStr, ok := strings.Cut(raw, ".")
	if !ok || bucketStr == "" || macStr == "" {
		return false
	}
	bucket, err := strconv.ParseInt(bucketStr, 10, 64)
	if err != nil || bucket < 0 {
		return false
	}
	currentBucket := now.Unix() / dpopNonceBucketSeconds
	if bucket < currentBucket-2 || bucket > currentBucket {
		return false
	}
	gotMAC, err := base64.RawURLEncoding.DecodeString(macStr)
	if err != nil || len(gotMAC) != sha256.Size {
		return false
	}
	mac := hmac.New(sha256.New, dpopHMACSecret())
	_, _ = mac.Write([]byte(agentID))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(strconv.FormatInt(bucket, 10)))
	return hmac.Equal(gotMAC, mac.Sum(nil))
}

// isClientStreamingProcedure reports whether the Connect RPC procedure uses a
// client-streaming or bidi-streaming request body (the only procedures where
// the Agent cannot buffer and hash the full HTTP body ahead of time).
func isClientStreamingProcedure(path string) bool {
	switch path {
	case "/komari.metrics.v1.MetricsService/StreamMetrics",
		"/komari.metrics.v1.MetricsService/UploadMetrics",
		"/komari.webssh.v1.WebSSHService/AttachSession",
		"/komari.file.v1.FileService/AttachFileOperations":
		return true
	default:
		return false
	}
}

type dpopJWTHeader struct {
	Typ string     `json:"typ"`
	Alg string     `json:"alg"`
	JWK dpopJWTJWK `json:"jwk"`
}

type dpopJWTJWK struct {
	Kty string          `json:"kty"`
	Crv string          `json:"crv"`
	X   string          `json:"x"`
	D   json.RawMessage `json:"d,omitempty"`
}

type dpopJWTClaims struct {
	JTI   string `json:"jti"`
	HTM   string `json:"htm"`
	HTU   string `json:"htu"`
	IAT   int64  `json:"iat"`
	ATH   string `json:"ath"`
	Nonce string `json:"nonce"`
	BH    string `json:"bh"`
	BM    string `json:"bm"`
	QSH   string `json:"qsh"`
}

func normalizeDPoPHTU(raw string) (string, error) {
	if raw == "" || strings.TrimSpace(raw) != raw || strings.ContainsAny(raw, "?#") {
		return "", errors.New("dpop: invalid htu claim")
	}
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", errors.New("dpop: invalid htu claim")
	}
	origin, err := requestmeta.NormalizeOrigin(strings.ToLower(u.Scheme) + "://" + u.Host)
	if err != nil {
		return "", errors.New("dpop: invalid htu origin")
	}
	p := u.EscapedPath()
	if p == "" {
		p = "/"
	}
	return origin + p, nil
}

func verifyDPoP(c *gin.Context, principal *rpc.Principal) error {
	proofHeaders := c.Request.Header.Values("DPoP")
	if len(proofHeaders) > 1 || (len(proofHeaders) == 1 && strings.Contains(proofHeaders[0], ",")) {
		return errors.New("dpop: multiple proof values are forbidden")
	}
	dpopProof := strings.TrimSpace(c.GetHeader("DPoP"))
	legacySig := strings.TrimSpace(c.GetHeader("X-Komari-Signature"))
	auth := strings.Fields(c.GetHeader("Authorization"))
	if len(auth) != 0 && strings.EqualFold(auth[0], "DPoP") && dpopProof == "" {
		c.Header("WWW-Authenticate", `DPoP error="invalid_dpop_proof"`)
		return errors.New("dpop: Authorization DPoP requires a DPoP proof")
	}

	var pubKey ed25519.PublicKey
	var requireJWT bool
	if principal != nil && principal.Type == rpc.PrincipalAgent && principal.ClientUUID != "" {
		pk, pinned, err := enrollment.ResolveAgentProofKey(principal.ClientUUID)
		if err != nil {
			// Legacy un-enrolled agents have no row in agent_credentials; only
			// allow unsigned passthrough when no credential row exists AND no
			// proof header was supplied. Any DB failure or corrupted key fails closed.
			if !errors.Is(err, gorm.ErrRecordNotFound) {
				return fmt.Errorf("dpop: resolve agent public key: %w", err)
			}
			if dpopProof == "" && legacySig == "" {
				return nil
			}
			return fmt.Errorf("dpop: resolve agent public key: %w", err)
		}
		if len(pk) != ed25519.PublicKeySize {
			return errors.New("dpop: invalid stored agent public key")
		}
		pubKey = pk
		requireJWT = pinned
		if dpopProof == "" && legacySig == "" {
			c.Header("DPoP-Nonce", currentDPoPNonce(principal.ClientUUID, time.Now()))
			c.Header("WWW-Authenticate", `DPoP error="invalid_dpop_proof"`)
			return errors.New("dpop: missing required DPoP proof for enrolled agent")
		}
	} else if dpopProof == "" && legacySig == "" {
		return nil
	} else {
		return errors.New("dpop: agent principal required for signed request")
	}

	if dpopProof != "" {
		if err := verifyRFC9449DPoP(c, principal.ClientUUID, pubKey, dpopProof); err != nil {
			return err
		}
		if !requireJWT {
			return enrollment.RequireAgentDPoPJWT(c.Request.Context(), principal.ClientUUID, pubKey)
		}
		return nil
	}
	if requireJWT || c.Request.URL.RawQuery != "" {
		c.Header("DPoP-Nonce", currentDPoPNonce(principal.ClientUUID, time.Now()))
		c.Header("WWW-Authenticate", `DPoP error="invalid_dpop_proof"`)
		return errors.New("dpop: enrolled agent must use a JWT proof for this request")
	}
	return verifyLegacyDPoP(c, principal.ClientUUID, pubKey, legacySig)
}

func verifyRFC9449DPoP(c *gin.Context, agentID string, pubKey ed25519.PublicKey, rawJWT string) error {
	now := time.Now()
	c.Header("DPoP-Nonce", currentDPoPNonce(agentID, now))

	if len(rawJWT) > maxDPoPJWTBytes {
		return errors.New("dpop: proof exceeds maximum size")
	}
	authHeader := strings.TrimSpace(c.GetHeader("Authorization"))
	parts := strings.SplitN(authHeader, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "DPoP") || strings.TrimSpace(parts[1]) == "" {
		c.Header("WWW-Authenticate", `DPoP error="invalid_token"`)
		return errors.New("dpop: Authorization DPoP scheme required")
	}
	token := strings.TrimSpace(parts[1])

	segments := strings.Split(rawJWT, ".")
	if len(segments) != 3 || segments[0] == "" || segments[1] == "" || segments[2] == "" {
		return errors.New("dpop: malformed compact JWS")
	}
	headerBytes, err := base64.RawURLEncoding.DecodeString(segments[0])
	if err != nil {
		return errors.New("dpop: invalid JWS header encoding")
	}
	claimsBytes, err := base64.RawURLEncoding.DecodeString(segments[1])
	if err != nil {
		return errors.New("dpop: invalid JWS claims encoding")
	}
	sigBytes, err := base64.RawURLEncoding.DecodeString(segments[2])
	if err != nil || len(sigBytes) != ed25519.SignatureSize {
		return errors.New("dpop: invalid JWS signature encoding")
	}

	var hdr dpopJWTHeader
	if err := json.Unmarshal(headerBytes, &hdr); err != nil {
		return errors.New("dpop: malformed JWS header JSON")
	}
	if !strings.EqualFold(hdr.Typ, "dpop+jwt") || hdr.Alg != "EdDSA" ||
		hdr.JWK.Kty != "OKP" || hdr.JWK.Crv != "Ed25519" || len(hdr.JWK.D) != 0 {
		return errors.New("dpop: unsupported JWS header parameters")
	}
	jwkPub, err := base64.RawURLEncoding.DecodeString(hdr.JWK.X)
	if err != nil || len(jwkPub) != ed25519.PublicKeySize {
		return errors.New("dpop: invalid OKP JWK public key")
	}
	if subtle.ConstantTimeCompare(jwkPub, pubKey) != 1 {
		return errors.New("dpop: JWK does not match enrolled agent public key")
	}

	signingInput := segments[0] + "." + segments[1]
	if !ed25519.Verify(pubKey, []byte(signingInput), sigBytes) {
		return errors.New("dpop: JWS signature verification failed")
	}

	var claims dpopJWTClaims
	if err := json.Unmarshal(claimsBytes, &claims); err != nil {
		return errors.New("dpop: malformed JWS claims JSON")
	}
	if claims.JTI == "" || len(claims.JTI) > maxNonceLength {
		return errors.New("dpop: missing or invalid jti claim")
	}
	if !strings.EqualFold(claims.HTM, c.Request.Method) || claims.HTM != c.Request.Method {
		return errors.New("dpop: htm method mismatch")
	}
	expectedOrigin, err := requestmeta.Origin(c.Request)
	if err != nil {
		return errors.New("dpop: cannot resolve canonical request origin")
	}
	reqPath := c.Request.URL.EscapedPath()
	if reqPath == "" {
		reqPath = "/"
	}
	gotHTU, err := normalizeDPoPHTU(claims.HTU)
	if err != nil || gotHTU != expectedOrigin+reqPath {
		return errors.New("dpop: htu claim mismatch")
	}
	// RFC 9449 leaves the query out of htu. Bind Connect query data separately.
	expectedQSH := ""
	if rawQuery := c.Request.URL.RawQuery; rawQuery != "" {
		queryDigest := sha256.Sum256([]byte(rawQuery))
		expectedQSH = base64.RawURLEncoding.EncodeToString(queryDigest[:])
	}
	if subtle.ConstantTimeCompare([]byte(claims.QSH), []byte(expectedQSH)) != 1 {
		return errors.New("dpop: query hash mismatch")
	}
	if math.Abs(float64(now.Unix()-claims.IAT)) > 120 {
		return errors.New("dpop: iat drift exceeds allowed window (120s)")
	}
	tokenDigest := sha256.Sum256([]byte(token))
	expectedATH := base64.RawURLEncoding.EncodeToString(tokenDigest[:])
	if subtle.ConstantTimeCompare([]byte(claims.ATH), []byte(expectedATH)) != 1 {
		return errors.New("dpop: ath access token hash mismatch")
	}

	allowStream := isClientStreamingProcedure(c.Request.URL.Path)
	if claims.BM != "" && claims.BM != "stream" {
		return errors.New("dpop: unsupported bm body mode")
	}
	if claims.BM == "stream" && !allowStream {
		return errors.New("dpop: stream body mode not permitted for unary RPC")
	}
	isStream := allowStream && (claims.BM == "stream" || c.Request.ContentLength < 0)

	var expectedBH string
	if isStream {
		emptyDigest := sha256.Sum256(nil)
		expectedBH = base64.RawURLEncoding.EncodeToString(emptyDigest[:])
	} else {
		var bodyBytes []byte
		if c.Request.Body != nil {
			bodyBytes, err = io.ReadAll(io.LimitReader(c.Request.Body, maxDPoPBodyBytes+1))
			if err != nil {
				return errors.New("dpop: failed to read request body")
			}
			if len(bodyBytes) > maxDPoPBodyBytes {
				return errors.New("dpop: request body exceeds maximum size")
			}
			c.Request.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		}
		bodyDigest := sha256.Sum256(bodyBytes)
		expectedBH = base64.RawURLEncoding.EncodeToString(bodyDigest[:])
	}
	if subtle.ConstantTimeCompare([]byte(claims.BH), []byte(expectedBH)) != 1 {
		return errors.New("dpop: bh body hash mismatch")
	}

	if !verifyDPoPServerNonce(agentID, claims.Nonce, now) {
		c.Header("WWW-Authenticate", `DPoP error="use_dpop_nonce"`)
		return errors.New("dpop: missing, invalid or expired server nonce")
	}

	jtiTTL := time.Until(time.Unix(claims.IAT, 0).Add(125 * time.Second))
	if jtiTTL < 120*time.Second {
		jtiTTL = 120 * time.Second
	}
	if !dpopReplayCache.CheckAndRecord(agentID, claims.JTI, jtiTTL) {
		return errors.New("dpop: jti replay detected")
	}
	return nil
}

func verifyLegacyDPoP(c *gin.Context, agentID string, pubKey ed25519.PublicKey, sig string) error {
	tsStr := strings.TrimSpace(c.GetHeader("X-Komari-Timestamp"))
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

	nonce := strings.TrimSpace(c.GetHeader("X-Komari-Nonce"))
	if nonce == "" || len(nonce) > maxNonceLength {
		return errors.New("dpop: missing or invalid X-Komari-Nonce header")
	}

	authHeader := c.GetHeader("Authorization")
	parts := strings.SplitN(authHeader, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || strings.TrimSpace(parts[1]) == "" {
		return errors.New("dpop: valid Bearer token required")
	}
	token := parts[1]
	tokenDigest := sha256.Sum256([]byte(token))
	tokenHash := hex.EncodeToString(tokenDigest[:])

	// Server-authoritative stream mode: never let callers downgrade unary or
	// server-streaming RPC body verification by sending X-Komari-Body-Mode: stream.
	isStream := isClientStreamingProcedure(c.Request.URL.Path) &&
		(c.GetHeader("X-Komari-Body-Mode") == "stream" || c.Request.ContentLength < 0)

	var bodyHash string
	if isStream {
		bodyDigest := sha256.Sum256(nil)
		bodyHash = hex.EncodeToString(bodyDigest[:])
	} else {
		var bodyBytes []byte
		var readErr error
		if c.Request.Body != nil {
			bodyBytes, readErr = io.ReadAll(io.LimitReader(c.Request.Body, maxDPoPBodyBytes+1))
			if readErr != nil {
				return errors.New("dpop: failed to read request body")
			}
			if len(bodyBytes) > maxDPoPBodyBytes {
				return errors.New("dpop: request body exceeds maximum size")
			}
			c.Request.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		}
		bodyDigest := sha256.Sum256(bodyBytes)
		bodyHash = hex.EncodeToString(bodyDigest[:])
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

	// Record nonce only after signature verification succeeds so an attacker
	// without the enrolled private key cannot pollute or exhaust replay capacity.
	nonceTTL := time.Until(time.Unix(ts, 0).Add(125 * time.Second))
	if nonceTTL < 120*time.Second {
		nonceTTL = 120 * time.Second
	}
	if !dpopReplayCache.CheckAndRecord(agentID, nonce, nonceTTL) {
		return errors.New("dpop: nonce replay detected")
	}

	return nil
}
