package requestscheme

import (
	"net/http"

	"github.com/komari-monitor/komari/pkg/requestmeta"
)

// IsHTTPS shares the explicit proxy trust boundary used by authentication.
func IsHTTPS(r *http.Request) bool {
	if r == nil {
		return false
	}
	if r.TLS != nil {
		return true
	}
	return requestmeta.ForwardedHTTPS(r)
}

// IsForwardedHTTPS reports whether a trusted local proxy marked the original
// request as HTTPS, independently of the proxy-to-Komari transport.
func IsForwardedHTTPS(r *http.Request) bool {
	return requestmeta.ForwardedHTTPS(r)
}
