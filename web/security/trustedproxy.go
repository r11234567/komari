package security

import (
	"net/http"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/pkg/requestmeta"
)

// TrustedProxyEnv names the environment variable that declares which peers may
// set X-Forwarded-For / X-Real-Ip on this deployment.
const TrustedProxyEnv = requestmeta.TrustedProxyEnv

// LoopbackProxies is the value to use for the common single-host deployment
// where a reverse proxy runs beside Komari and connects over loopback.
const LoopbackProxies = "127.0.0.1,::1"

// ConfigureRequestMetadata is shared by normal, install and upgrade listeners.
func ConfigureRequestMetadata(engine *gin.Engine) error {
	if err := requestmeta.ValidateConfig(); err != nil {
		return err
	}
	if err := ConfigureTrustedProxies(engine, TrustedProxiesFromEnv()); err != nil {
		return err
	}
	// Validate the destination before authentication, including REST and the
	// install/upgrade listeners. RPC's adapter later uses this same resolver
	// for passkey and workspace metadata, never a second forwarding policy.
	engine.Use(func(c *gin.Context) {
		if _, err := requestmeta.Origin(c.Request); err != nil {
			AbortRequest(c, http.StatusForbidden, "invalid external request origin")
			return
		}
		c.Next()
	})
	return nil
}

// ConfigureTrustedProxies tells gin which peers are allowed to declare the
// real client address.
//
// Why this is not simply hardcoded: gin's default trusts every proxy
// (0.0.0.0/0 and ::/0), and its header walk stops at the first untrusted hop,
// so with everything trusted it returns the *leftmost* X-Forwarded-For entry -
// a value the client itself supplies. ClientIP() is therefore attacker-chosen
// unless this is set, which lets a caller rotate the value to get a fresh rate
// limit bucket per request, or reuse somebody else's address to drain theirs.
// The same value is recorded as the source address in the audit log and on
// login sessions.
//
// The default is no trusted proxies. Behind a proxy, operators must declare
// the actual transport peers. A private address is not implicit trust.
//
//	unset or empty  trust no proxy
//	"none"          trust no proxy; the peer address is always the client
//	comma list      trust exactly these hosts or CIDRs
//
// Returns an error only for a malformed list, so a typo surfaces at startup
// rather than silently reverting to trusting everything.
func ConfigureTrustedProxies(engine *gin.Engine, value string) error {
	proxies, err := requestmeta.TrustedProxies(value)
	if err != nil {
		return err
	}
	return engine.SetTrustedProxies(proxies)
}

// TrustedProxiesFromEnv reads the configured value, or the empty string when
// unset.
func TrustedProxiesFromEnv() string {
	return strings.TrimSpace(os.Getenv(TrustedProxyEnv))
}
