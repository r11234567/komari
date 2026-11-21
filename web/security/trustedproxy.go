package security

import (
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
const LoopbackProxies = requestmeta.LoopbackProxies

// ConfigureRequestMetadata is shared by normal, install and upgrade listeners.
func ConfigureRequestMetadata(engine *gin.Engine) error {
	policy, err := requestmeta.FromEnvironment()
	if err != nil {
		return err
	}
	return bindMetadataPolicy(engine, policy)
}

// ConfigureRequestMetadataForOrigin binds a listener-specific canonical origin
// (such as EffectivePanelOrigin on the Panel listener or EffectivePublicOrigin
// on the Public listener) without cross-contaminating split listeners.
func ConfigureRequestMetadataForOrigin(engine *gin.Engine, canonicalOrigin string) error {
	policy, err := requestmeta.NewPolicy(TrustedProxiesFromEnv(), canonicalOrigin)
	if err != nil {
		return err
	}
	return bindMetadataPolicy(engine, policy)
}

func bindMetadataPolicy(engine *gin.Engine, policy *requestmeta.Policy) error {
	if err := engine.SetTrustedProxies(policy.ProxyAddresses()); err != nil {
		return err
	}
	engine.Use(func(c *gin.Context) {
		c.Request = policy.BindRequest(c.Request)
		c.Next()
	})
	return nil
}

// ConfigureTrustedProxies tells gin which peers are allowed to declare the
// real client address.
//
// The default trusts only local loopback proxies, preserving a same-host Nginx
// deployment. Containers/private networks require the actual peer addresses.
//
//	unset or empty  trust 127.0.0.1 and ::1
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
