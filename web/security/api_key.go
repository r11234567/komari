package security

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// Fixed-length digests avoid length-dependent secret comparison. Hashing is
// appropriate for high-entropy API credentials, not for human passwords.
func APIKeyMatches(authorization, configured string) bool {
	if len(configured) < 12 {
		return false
	}
	want := sha256.Sum256([]byte("Bearer " + configured))
	got := sha256.Sum256([]byte(authorization))
	return subtle.ConstantTimeCompare(want[:], got[:]) == 1
}

type apiKeyContextKey struct{}

func WithAPIKeyAuthentication(r *http.Request, authenticated bool) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), apiKeyContextKey{}, authenticated))
}

// AuthSourceIP never trusts caller-selected forwarding headers when the
// deployment has left gin's historical trust-all proxy setting in place.
func AuthSourceIP(c *gin.Context) string {
	if !ClientIPIsSpoofable(TrustedProxiesFromEnv()) {
		return c.ClientIP()
	}
	host, _, err := net.SplitHostPort(c.Request.RemoteAddr)
	if err != nil {
		return strings.TrimSpace(c.Request.RemoteAddr)
	}
	return host
}

type APIKeyAttemptLimiter struct{ budget *RateLimitController }

func NewAPIKeyAttemptLimiter() *APIKeyAttemptLimiter {
	return &APIKeyAttemptLimiter{budget: NewRateLimitController(true)}
}

// Ten failures can burst; one failure replenishes every six seconds. This
// independent budget remains enabled when ordinary request limiting is off.
func (l *APIKeyAttemptLimiter) Allow(source string, now time.Time) (bool, time.Duration) {
	l.budget.mu.Lock()
	defer l.budget.mu.Unlock()
	bucket, exists := l.budget.buckets["api-auth:"+source]
	if !exists {
		return true, 0
	}
	tokens := bucket.tokens + max(0, now.Sub(bucket.seen).Seconds())/6
	if tokens >= 1 {
		return true, 0
	}
	return false, retryAfterFor(tokens, 1.0/6)
}

func (l *APIKeyAttemptLimiter) Failure(source string, now time.Time) {
	l.budget.allow("api-auth:"+source, 1.0/6, 10, now)
}

type APITokenUsageLimiter struct{ budget *RateLimitController }

func NewAPITokenUsageLimiter() *APITokenUsageLimiter {
	return &APITokenUsageLimiter{budget: NewRateLimitController(true)}
}

// Valid scoped credentials also have a budget, keyed by token rather than IP.
// Costly history reads share a stricter lane; changing IP does not reset it.
func (l *APITokenUsageLimiter) Allow(id, path string, now time.Time) (bool, time.Duration) {
	if isHistoricalReadPath(path) {
		if ok, retry := l.budget.allow("api-history:"+id, 1, 4, now); !ok {
			return false, retry
		}
	}
	return l.budget.allow("api-use:"+id, 10, 30, now)
}
