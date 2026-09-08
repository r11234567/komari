package security

import (
	"os"
	"strings"

	"github.com/gin-gonic/gin"
)

// TrustedProxyEnv names the environment variable that declares which peers may
// set X-Forwarded-For / X-Real-Ip on this deployment.
const TrustedProxyEnv = "KOMARI_TRUSTED_PROXIES"

// LoopbackProxies is the value to use for the common single-host deployment
// where a reverse proxy runs beside Komari and connects over loopback.
const LoopbackProxies = "127.0.0.1,::1"

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
// It is not hardcoded to loopback because that is only correct when a proxy
// terminates in front of Komari. A deployment exposed directly, or fronted by a
// proxy on another host or in another container, would then attribute every
// request to the proxy's address or to a single shared bucket. So the default
// preserves the historical behaviour and the safe configuration is opt-in;
// value semantics:
//
//	unset or empty  keep gin's default (trust every proxy, headers honoured)
//	"none"          trust no proxy; the peer address is always the client
//	comma list      trust exactly these hosts or CIDRs
//
// Returns an error only for a malformed list, so a typo surfaces at startup
// rather than silently reverting to trusting everything.
func ConfigureTrustedProxies(engine *gin.Engine, value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	if strings.EqualFold(value, "none") {
		// A nil list makes gin ignore forwarding headers entirely and report the
		// transport peer, which is correct for a directly exposed deployment.
		return engine.SetTrustedProxies(nil)
	}
	proxies := make([]string, 0, 4)
	for _, entry := range strings.Split(value, ",") {
		if trimmed := strings.TrimSpace(entry); trimmed != "" {
			proxies = append(proxies, trimmed)
		}
	}
	if len(proxies) == 0 {
		return nil
	}
	return engine.SetTrustedProxies(proxies)
}

// TrustedProxiesFromEnv reads the configured value, or the empty string when
// unset.
func TrustedProxiesFromEnv() string {
	return strings.TrimSpace(os.Getenv(TrustedProxyEnv))
}

// ClientIPIsSpoofable reports whether the configured value leaves the client
// address under caller control, so startup can warn once about it.
func ClientIPIsSpoofable(value string) bool {
	return strings.TrimSpace(value) == ""
}
