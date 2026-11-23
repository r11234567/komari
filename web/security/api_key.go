package security

import (
	"time"

	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/pkg/requestmeta"
)

// AuthSourceIP shares the raw HTTP and Gin trust policy.
func AuthSourceIP(c *gin.Context) string {
	return requestmeta.ClientIP(c.Request)
}

type APIKeyAttemptLimiter struct {
	budget     *RateLimitController
	probeTotal *RateLimitController
}

func NewAPIKeyAttemptLimiter() *APIKeyAttemptLimiter {
	return &APIKeyAttemptLimiter{
		budget:     NewRateLimitController(true),
		probeTotal: NewRateLimitController(true),
	}
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

// AllowUnknownAgentProbe charges both budgets before an unclassified token
// can reach the credential table. The global budget also bounds rotating IPs.
func (l *APIKeyAttemptLimiter) AllowUnknownAgentProbe(sourceIP string, now time.Time) (bool, time.Duration) {
	if ok, retry := l.budget.allow("agent-probe:"+sourceIP, 1.0/3, 20, now); !ok {
		return false, retry
	}
	return l.probeTotal.allow("agent-probe-total", 8, 40, now)
}

type APITokenUsageLimiter struct{ budget *RateLimitController }

func NewAPITokenUsageLimiter() *APITokenUsageLimiter {
	return &APITokenUsageLimiter{budget: NewRateLimitController(true)}
}

// Valid scoped credentials also have a budget, keyed by token rather than IP.
// Costly history reads share a stricter lane; changing IP does not reset it.
func (l *APITokenUsageLimiter) Allow(id, path string, now time.Time) (bool, time.Duration) {
	if isHistoricalReadPath(path) {
		if ok, retry := l.budget.allow("api-history:"+id, historyReadRate, historyReadBurst, now); !ok {
			return false, retry
		}
	}
	return l.budget.allow("api-use:"+id, 40, 160, now)
}
