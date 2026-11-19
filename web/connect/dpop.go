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
	"gorm.io/gorm"
)

const (
	maxDPoPNonceEntries    = 65536
	maxDPoPPerSubject      = 1024
	maxRefreshNonceEntries = 16384
	maxRefreshPerSubject   = 256
	maxNonceLength         = 128
	maxDPoPBodyBytes       = 10 << 20
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

func verifyDPoP(c *gin.Context, principal *rpc.Principal) error {
	sig := strings.TrimSpace(c.GetHeader("X-Komari-Signature"))

	var pubKey ed25519.PublicKey
	if principal != nil && principal.Type == rpc.PrincipalAgent && principal.ClientUUID != "" {
		pk, err := enrollment.ResolveAgentPublicKey(principal.ClientUUID)
		if err != nil {
			// Legacy un-enrolled agents have no row in agent_credentials; only
			// allow unsigned passthrough when no credential row exists AND no
			// signature was supplied. Any DB failure or corrupted key fails closed.
			if !errors.Is(err, gorm.ErrRecordNotFound) {
				return fmt.Errorf("dpop: resolve agent public key: %w", err)
			}
			if sig == "" {
				return nil
			}
			return fmt.Errorf("dpop: resolve agent public key: %w", err)
		}
		if len(pk) != ed25519.PublicKeySize {
			return errors.New("dpop: invalid stored agent public key")
		}
		pubKey = pk
		if sig == "" {
			return errors.New("dpop: missing required X-Komari-Signature header for enrolled agent")
		}
	} else if sig == "" {
		return nil
	} else {
		return errors.New("dpop: agent principal required for signed request")
	}

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
	// Retain the nonce at least until the signature's own timestamp window
	// expires (ts + 120s + 5s margin) so future-skewed signatures cannot be
	// replayed after a shorter fixed retention elapses.
	nonceTTL := time.Until(time.Unix(ts, 0).Add(125 * time.Second))
	if nonceTTL < 120*time.Second {
		nonceTTL = 120 * time.Second
	}
	if !dpopReplayCache.CheckAndRecord(principal.ClientUUID, nonce, nonceTTL) {
		return errors.New("dpop: nonce replay detected")
	}

	return nil
}
