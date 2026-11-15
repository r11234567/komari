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

// Runs before identity dispatch. A supplied bearer credential never
// falls back to a browser session when it is invalid, expired or revoked.
func APIAuthenticationMiddleware(pluginRoute ...func(method, pattern string) bool) gin.HandlerFunc {
	limiter := security.NewAPIKeyAttemptLimiter()
	usage := security.NewAPITokenUsageLimiter()
	return func(c *gin.Context) {
		header := c.GetHeader("Authorization")
		if !strings.HasPrefix(header, "Bearer ") {
			c.Next()
			return
		}
		secret := strings.TrimPrefix(header, "Bearer ")
		// Official plugins may implement their own Bearer authentication on
		// their own routes. Do not reinterpret that foreign credential as a
		// failed Komari key. Namespaced Komari credentials are never exempted.
		if len(pluginRoute) > 0 && pluginRoute[0](c.Request.Method, c.FullPath()) &&
			!strings.HasPrefix(secret, apitokens.Prefix) && !strings.HasPrefix(secret, "komari-") && !isApiKeyValid(header) {
			c.Next()
			return
		}
		// Existing enrolled device credentials have their own identity and DPoP
		// gates. They must not spend the API credential budget on every report.
		if !strings.HasPrefix(secret, apitokens.Prefix) {
			if id, err := enrollment.ResolveAccessToken(secret); err == nil && id != "" {
				SetPrincipal(c, rpc.NewAgentPrincipal(id))
				c.Request = security.WithAPIKeyAuthentication(c.Request, false)
				c.Next()
				return
			}
			if enrollment.IsInstallToken(secret) {
				if id, err := enrollment.RedeemInstallToken(secret); err == nil && id != "" {
					SetPrincipal(c, rpc.NewAgentPrincipal(id))
					c.Request = security.WithAPIKeyAuthentication(c.Request, false)
					c.Next()
					return
				}
			}
		}
		if ok, retry := limiter.Allow(security.AuthSourceIP(c), time.Now()); !ok {
			c.Header("Retry-After", strconv.Itoa(int(math.Ceil(retry.Seconds()))))
			security.AbortRequest(c, http.StatusTooManyRequests, "API authentication rate limit exceeded")
			return
		}
		var principal *rpc.Principal
		if strings.HasPrefix(secret, apitokens.Prefix) {
			var err error
			principal, err = apitokens.Authenticate(c.Request.Context(), secret, security.AuthSourceIP(c))
			if err != nil {
				if !errors.Is(err, apitokens.ErrInvalid) {
					security.AbortRequest(c, http.StatusServiceUnavailable, "API authentication unavailable")
					return
				}
				limiter.Failure(security.AuthSourceIP(c), time.Now())
				security.AbortRequest(c, http.StatusUnauthorized, "invalid API credential")
				return
			}
		} else if isApiKeyValid(header) {
			principal = rpc.NewAPIKeyPrincipal()
		} else {
			limiter.Failure(security.AuthSourceIP(c), time.Now())
			security.AbortRequest(c, http.StatusUnauthorized, "invalid API credential")
			return
		}
		SetPrincipal(c, principal)
		security.SetPrincipalHeader(c, principal)
		if !principal.AllowsAPIProcedure(c.Request.URL.Path) {
			security.AbortRequest(c, http.StatusForbidden, "API token lacks permission for this procedure")
			return
		}
		if principal.APIToken != nil {
			if ok, retry := usage.Allow(principal.APIToken.ID, c.Request.URL.Path, time.Now()); !ok {
				c.Header("Retry-After", strconv.Itoa(int(math.Ceil(retry.Seconds()))))
				security.AbortRequest(c, http.StatusTooManyRequests, "API token request budget exceeded")
				return
			}
		}
		c.Request = security.WithAPIKeyAuthentication(c.Request, true)
		c.Next()
	}
}
