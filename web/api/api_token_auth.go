package api

import (
	"context"
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
	"gorm.io/gorm"
)

const (
	unknownMissCacheTTL        = 10 * time.Second
	unknownMissCacheMaxEntries = 4096
	unknownProbeConcurrency    = 4
	unknownProbeTimeout        = 250 * time.Millisecond
)

type unknownMissGuard struct {
	mu     sync.Mutex
	misses map[[sha256.Size]byte]time.Time
	active chan struct{}
}

func newUnknownMissGuard() *unknownMissGuard {
	return &unknownMissGuard{
		misses: make(map[[sha256.Size]byte]time.Time),
		active: make(chan struct{}, unknownProbeConcurrency),
	}
}

func (g *unknownMissGuard) knownMiss(digest [sha256.Size]byte, now time.Time) bool {
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

func (g *unknownMissGuard) recordMiss(digest [sha256.Size]byte, now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.misses) >= unknownMissCacheMaxEntries {
		for k := range g.misses {
			delete(g.misses, k)
			break
		}
	}
	g.misses[digest] = now.Add(unknownMissCacheTTL)
}

func (g *unknownMissGuard) lookup(ctx context.Context, limiter *security.APIKeyAttemptLimiter, sourceIP, token string, now time.Time) (string, bool, time.Duration, error) {
	select {
	case g.active <- struct{}{}:
		defer func() { <-g.active }()
	default:
		return "", false, time.Second, nil
	}
	if ok, retry := limiter.AllowUnknownAgentProbe(sourceIP, now); !ok {
		return "", false, retry, nil
	}
	ctx, cancel := context.WithTimeout(ctx, unknownProbeTimeout)
	defer cancel()
	uuid, known, err := enrollment.LookupAccessTokenContext(ctx, token)
	return uuid, known, 0, err
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
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	storedDigests, _ := enrollment.StoredAccessTokenDigests(ctx)
	cancel()
	return func(c *gin.Context) {
		scheme, secret, ok := requestAuthCredential(c.GetHeader("Authorization"))
		if !ok {
			c.Next()
			return
		}
		if scheme == "DPoP" && (!strings.HasPrefix(c.FullPath(), "/komari.") ||
			strings.HasPrefix(secret, apitokens.Prefix) || enrollment.IsInstallToken(secret)) {
			security.AbortRequest(c, http.StatusUnauthorized, "DPoP credentials require an enrolled Agent Connect procedure")
			return
		}
		now := time.Now()
		plausible := isPlausibleAgentAccessToken(secret)
		var digest [sha256.Size]byte
		var stored bool
		if plausible {
			digest = sha256.Sum256([]byte(secret))
			_, stored = storedDigests[digest]
		}
		// Official plugins may implement their own Bearer authentication on
		// their own routes. Do not reinterpret that foreign credential as a
		// failed Komari key or hand it to the legacy identity lookup.
		if len(pluginRoute) > 0 && pluginRoute[0](c.Request.Method, c.FullPath()) &&
			!stored && !strings.HasPrefix(secret, apitokens.Prefix) && !strings.HasPrefix(secret, "komari-") {
			SetPrincipal(c, rpc.NewAnonymousPrincipal())
			c.Next()
			return
		}

		sourceIP := security.AuthSourceIP(c)
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
			failureLane := "unknown:"
			if plausible && !missGuard.knownMiss(digest, now) {
				var uuid string
				var known bool
				var err error
				if stored || enrollment.IsIssuedAccessToken(secret, now) {
					uuid, known, err = enrollment.LookupAccessTokenContext(c.Request.Context(), secret)
				} else {
					var retry time.Duration
					uuid, known, retry, err = missGuard.lookup(c.Request.Context(), limiter, sourceIP, secret, now)
					if retry > 0 {
						abortRateLimited(c, retry)
						return
					}
				}
				switch {
				case err == nil && uuid != "":
					SetPrincipal(c, rpc.NewAgentPrincipal(uuid))
					c.Next()
					return
				case known:
					failureLane = "agent:"
				case errors.Is(err, gorm.ErrRecordNotFound):
					missGuard.recordMiss(digest, now)
				default:
					security.AbortRequest(c, http.StatusServiceUnavailable, "API authentication unavailable")
					return
				}
			}

			failureKey := failureLane + sourceIP
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
