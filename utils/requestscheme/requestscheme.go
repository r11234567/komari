package requestscheme

import (
	"net/http"

	"github.com/komari-monitor/komari/pkg/requestmeta"
)

// IsHTTPS reports whether the request arrived over TLS or through an
// explicitly trusted reverse proxy that terminated HTTPS.
func IsHTTPS(r *http.Request) bool {
	return r != nil && (r.TLS != nil || IsForwardedHTTPS(r))
}

// IsForwardedHTTPS delegates forwarded-HTTPS detection to the shared requestmeta
// proxy policy so forwarding trust never diverges across listeners.
func IsForwardedHTTPS(r *http.Request) bool {
	return requestmeta.ForwardedHTTPS(r)
}
