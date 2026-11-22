package api

import (
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/database/apitokens"
	"github.com/komari-monitor/komari/database/enrollment"
	"github.com/komari-monitor/komari/pkg/rpc"
	"github.com/komari-monitor/komari/web/security"
)

// Runs before CORS and identity dispatch. A supplied bearer credential never
// falls back to a browser session when it is invalid, expired or revoked.
func APIAuthenticationMiddleware(pluginRoute ...func(method, pattern string) bool) gin.HandlerFunc {
	limiter := security.NewAPIKeyAttemptLimiter()
	usage := security.NewAPITokenUsageLimiter()
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
		// Existing enrolled device credentials have their own identity and DPoP
		// gates. They must not spend the API credential budget on every report.
		if !strings.HasPrefix(secret, apitokens.Prefix) {
			if principal := agentPrincipal(secret); principal.Type == rpc.PrincipalAgent {
				SetPrincipal(c, principal)
				c.Next()
				return
			}
		}
		// Separate failure budgets so expired/revoked Agent bursts behind a NAT
		// never starve PAT logins or unknown brute-force probes.
		family := "unknown"
		if strings.HasPrefix(secret, apitokens.Prefix) {
			family = "pat"
		} else if enrollment.IsInstallToken(secret) || enrollment.HasAccessTokenRecord(secret) {
			family = "agent"
		}
		failureKey := family + ":" + sourceIP
		if ok, retry := limiter.Allow(failureKey, time.Now()); !ok {
			c.Header("Retry-After", strconv.Itoa(int(math.Ceil(retry.Seconds()))))
			security.AbortRequest(c, http.StatusTooManyRequests, "API authentication rate limit exceeded")
			return
		}
		if !strings.HasPrefix(secret, apitokens.Prefix) {
			limiter.Failure(failureKey, time.Now())
			security.AbortRequest(c, http.StatusUnauthorized, "invalid API credential")
			return
		}
		principal, err := apitokens.Authenticate(c.Request.Context(), secret, sourceIP)
		if err != nil {
			if !errors.Is(err, apitokens.ErrInvalid) {
				security.AbortRequest(c, http.StatusServiceUnavailable, "API authentication unavailable")
				return
			}
			limiter.Failure(failureKey, time.Now())
			security.AbortRequest(c, http.StatusUnauthorized, "invalid API credential")
			return
		}
		SetPrincipal(c, principal)
		security.SetPrincipalHeader(c, principal)
		if !principal.AllowsAPIProcedure(c.Request.URL.Path) {
			security.AbortRequest(c, http.StatusForbidden, "API token lacks permission for this procedure")
			return
		}
		if ok, retry := usage.Allow(principal.APIToken.ID, c.Request.URL.Path, time.Now()); !ok {
			c.Header("Retry-After", strconv.Itoa(int(math.Ceil(retry.Seconds()))))
			security.AbortRequest(c, http.StatusTooManyRequests, "API token request budget exceeded")
			return
		}
		c.Next()
	}
}
