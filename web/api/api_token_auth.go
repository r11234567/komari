package api

import (
	"crypto/sha256"
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/database/apitokens"
	"github.com/komari-monitor/komari/database/enrollment"
	"github.com/komari-monitor/komari/pkg/rpc"
	"github.com/komari-monitor/komari/web/security"
)

const (
	unknownMissCacheTTL        = 10 * time.Second
	unknownMissCacheMaxEntries = 4096
	unknownMissProbeBurst      = 20.0
	unknownMissProbeRefillSec  = 3.0 // 1 distinct uncached miss probe replenishes every 3s
)

type unknownMissBucket struct {
	tokens float64
	seen   time.Time
}

type unknownMissGuard struct {
	mu      sync.Mutex
	misses  map[[32]byte]time.Time
	buckets map[string]unknownMissBucket
}

func newUnknownMissGuard() *unknownMissGuard {
	return &unknownMissGuard{
		misses:  make(map[[32]byte]time.Time),
		buckets: make(map[string]unknownMissBucket),
	}
}

func (g *unknownMissGuard) knownMiss(digest [32]byte, now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	exp, ok := g.misses[digest]
	if !ok {
		return false
	}
	if !now.Before(exp) {
		delete(g.misses, digest)
		return false
	}
	return true
}

func (g *unknownMissGuard) allowMissProbe(sourceIP string, now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	b, exists := g.buckets[sourceIP]
	if !exists {
		return true
	}
	tokens := min(unknownMissProbeBurst, b.tokens+max(0, now.Sub(b.seen).Seconds())/unknownMissProbeRefillSec)
	return tokens >= 1
}

func (g *unknownMissGuard) recordMiss(sourceIP string, digest [32]byte, now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.misses) >= unknownMissCacheMaxEntries {
		for k, exp := range g.misses {
			if !now.Before(exp) {
				delete(g.misses, k)
			}
		}
		for k := range g.misses {
			if len(g.misses) < unknownMissCacheMaxEntries {
				break
			}
			delete(g.misses, k)
		}
	}
	g.misses[digest] = now.Add(unknownMissCacheTTL)

	if len(g.buckets) >= unknownMissCacheMaxEntries {
		if _, exists := g.buckets[sourceIP]; !exists {
			for k, b := range g.buckets {
				if now.Sub(b.seen) > time.Minute || len(g.buckets) >= unknownMissCacheMaxEntries {
					delete(g.buckets, k)
					if len(g.buckets) < unknownMissCacheMaxEntries {
						break
					}
				}
			}
		}
	}
	b, exists := g.buckets[sourceIP]
	tokens := unknownMissProbeBurst
	if exists {
		tokens = min(unknownMissProbeBurst, b.tokens+max(0, now.Sub(b.seen).Seconds())/unknownMissProbeRefillSec)
	}
	g.buckets[sourceIP] = unknownMissBucket{
		tokens: max(0, tokens-1),
		seen:   now,
	}
}

func isPlausibleAgentAccessToken(secret string) bool {
	if len(secret) < 16 || len(secret) > 128 {
		return false
	}
	for _, r := range secret {
		if unicode.IsSpace(r) {
			return false
		}
	}
	return true
}

func abortRateLimited(c *gin.Context, retry time.Duration) {
	c.Header("Retry-After", strconv.Itoa(max(1, int(math.Ceil(retry.Seconds())))))
	security.AbortRequest(c, http.StatusTooManyRequests, "API authentication rate limit exceeded")
}

// Runs before CORS and identity dispatch. A supplied bearer credential never
// falls back to a browser session when it is invalid, expired or revoked.
func APIAuthenticationMiddleware(pluginRoute ...func(method, pattern string) bool) gin.HandlerFunc {
	limiter := security.NewAPIKeyAttemptLimiter()
	usage := security.NewAPITokenUsageLimiter()
	missGuard := newUnknownMissGuard()
	return func(c *gin.Context) {
		secret, bearer := requestBearer(c.GetHeader("Authorization"))
		if !bearer {
			c.Next()
			return
		}
		// Official plugins may implement their own Bearer authentication on
		// their own routes. Do not reinterpret that foreign credential as a
		// failed Komari key. Namespaced Komari credentials are never exempted.
		if len(pluginRoute) > 0 && pluginRoute[0](c.Request.Method, c.FullPath()) &&
			!strings.HasPrefix(secret, apitokens.Prefix) && !strings.HasPrefix(secret, "komari-") {
			c.Next()
			return
		}

		sourceIP := security.AuthSourceIP(c)
		now := time.Now()
		isPAT := strings.HasPrefix(secret, apitokens.Prefix)
		isInstall := !isPAT && enrollment.IsInstallToken(secret)

		if isInstall {
			failureKey := "agent:" + sourceIP
			if ok, retry := limiter.Allow(failureKey, now); !ok {
				abortRateLimited(c, retry)
				return
			}
			if principal := agentPrincipal(secret); principal.Type == rpc.PrincipalAgent {
				SetPrincipal(c, principal)
				c.Next()
				return
			}
			limiter.Failure(failureKey, now)
			security.AbortRequest(c, http.StatusUnauthorized, "invalid API credential")
			return
		}

		if !isPAT {
			digest := sha256.Sum256([]byte(secret))
			skipSQL := !isPlausibleAgentAccessToken(secret) ||
				missGuard.knownMiss(digest, now) ||
				!missGuard.allowMissProbe(sourceIP, now)
			if !skipSQL {
				uuid, knownRecord, err := enrollment.LookupAccessToken(secret)
				if err == nil && uuid != "" {
					// Valid enrolled Agent credentials bypass unknown:<ip> limits
					// even behind a noisy NAT, while invalid/expired keys below stay metered.
					SetPrincipal(c, rpc.NewAgentPrincipal(uuid))
					c.Next()
					return
				}
				if knownRecord {
					failureKey := "agent:" + sourceIP
					if ok, retry := limiter.Allow(failureKey, now); !ok {
						abortRateLimited(c, retry)
						return
					}
					limiter.Failure(failureKey, now)
					security.AbortRequest(c, http.StatusUnauthorized, "invalid API credential")
					return
				}
				missGuard.recordMiss(sourceIP, digest, now)
			}

			failureKey := "unknown:" + sourceIP
			if ok, retry := limiter.Allow(failureKey, now); !ok {
				abortRateLimited(c, retry)
				return
			}
			limiter.Failure(failureKey, now)
			security.AbortRequest(c, http.StatusUnauthorized, "invalid API credential")
			return
		}

		failureKey := "pat:" + sourceIP
		if ok, retry := limiter.Allow(failureKey, now); !ok {
			abortRateLimited(c, retry)
			return
		}
		principal, err := apitokens.Authenticate(c.Request.Context(), secret, sourceIP)
		if err != nil {
			if !errors.Is(err, apitokens.ErrInvalid) {
				security.AbortRequest(c, http.StatusServiceUnavailable, "API authentication unavailable")
				return
			}
			limiter.Failure(failureKey, now)
			security.AbortRequest(c, http.StatusUnauthorized, "invalid API credential")
			return
		}
		SetPrincipal(c, principal)
		security.SetPrincipalHeader(c, principal)
		if !principal.AllowsAPIProcedure(c.Request.URL.Path) {
			security.AbortRequest(c, http.StatusForbidden, "API token lacks permission for this procedure")
			return
		}
		if ok, retry := usage.Allow(principal.APIToken.ID, c.Request.URL.Path, now); !ok {
			c.Header("Retry-After", strconv.Itoa(int(math.Ceil(retry.Seconds()))))
			security.AbortRequest(c, http.StatusTooManyRequests, "API token request budget exceeded")
			return
		}
		c.Next()
	}
}
