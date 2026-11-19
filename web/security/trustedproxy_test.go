package security

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// clientIPUnder reports what gin resolves as the client address for a request
// arriving from peer with the given forwarding header, under one configuration.
func clientIPUnder(t *testing.T, trusted, peer, forwarded string) string {
	t.Helper()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	if err := ConfigureTrustedProxies(engine, trusted); err != nil {
		t.Fatalf("ConfigureTrustedProxies(%q): %v", trusted, err)
	}
	var resolved string
	engine.GET("/api/probe", func(c *gin.Context) {
		resolved = c.ClientIP()
		c.Status(http.StatusNoContent)
	})
	request := httptest.NewRequest(http.MethodGet, "/api/probe", nil)
	request.RemoteAddr = peer
	if forwarded != "" {
		request.Header.Set("X-Forwarded-For", forwarded)
	}
	engine.ServeHTTP(httptest.NewRecorder(), request)
	return resolved
}

func TestDefaultConfigurationLeavesClientIPSpoofable(t *testing.T) {
	// This documents the hazard the option exists to close: with gin's default
	// every proxy is trusted, so the header walk runs to the leftmost entry -
	// a value the caller chose. Left unaddressed it lets a caller mint a fresh
	// rate limit bucket per request, or drain someone else's.
	got := clientIPUnder(t, "", "203.0.113.9:5555", "198.51.100.7")
	if got != "198.51.100.7" {
		t.Fatalf("client IP = %q, want the spoofed header value under the permissive default", got)
	}
	if !ClientIPIsSpoofable("") {
		t.Fatal("the default configuration was not reported as spoofable")
	}
}

func TestNoneIgnoresForwardingHeaders(t *testing.T) {
	got := clientIPUnder(t, "none", "203.0.113.9:5555", "198.51.100.7")
	if got != "203.0.113.9" {
		t.Fatalf("client IP = %q, want the transport peer", got)
	}
	if ClientIPIsSpoofable("none") {
		t.Fatal(`"none" was reported as spoofable`)
	}
}

func TestLoopbackProxyIsHonouredOnlyFromLoopback(t *testing.T) {
	// The reverse proxy's own hop is trusted, so the address it appended wins.
	got := clientIPUnder(t, LoopbackProxies, "127.0.0.1:5555", "198.51.100.7")
	if got != "198.51.100.7" {
		t.Fatalf("client IP from a trusted proxy = %q, want the forwarded address", got)
	}
	// The same header from an untrusted peer must not be believed, otherwise
	// bypassing the proxy would restore full spoofability.
	got = clientIPUnder(t, LoopbackProxies, "203.0.113.9:5555", "198.51.100.7")
	if got != "203.0.113.9" {
		t.Fatalf("client IP from an untrusted peer = %q, want the transport peer", got)
	}
}

func TestTrustedProxyChainStopsAtTheFirstUntrustedHop(t *testing.T) {
	// Two hops: the client's fabricated entry, then the address the trusted
	// proxy observed. Only the latter is credible.
	got := clientIPUnder(t, LoopbackProxies, "127.0.0.1:5555", "10.9.9.9, 198.51.100.7")
	if got != "198.51.100.7" {
		t.Fatalf("client IP = %q, want the address the trusted proxy appended", got)
	}
}

func TestCIDRAndWhitespaceHandling(t *testing.T) {
	got := clientIPUnder(t, " 10.0.0.0/8 , ::1 ", "10.1.2.3:5555", "198.51.100.7")
	if got != "198.51.100.7" {
		t.Fatalf("client IP = %q, want the forwarded address for a CIDR-trusted peer", got)
	}
	if ClientIPIsSpoofable(" 10.0.0.0/8 ") {
		t.Fatal("an explicit list was reported as spoofable")
	}
}

func TestEmptyListFallsBackToDefault(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	// A value of only separators carries no instruction; it must not be read as
	// "trust nothing", which would silently change every client address.
	if err := ConfigureTrustedProxies(engine, " , , "); err != nil {
		t.Fatalf("ConfigureTrustedProxies: %v", err)
	}
}

func TestMalformedListIsRejected(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	// A typo must surface at startup rather than quietly leaving every proxy
	// trusted, which is the insecure state.
	if err := ConfigureTrustedProxies(engine, "not-an-address"); err == nil {
		t.Fatal("a malformed proxy list was accepted")
	}
}

func TestNoneIsCaseInsensitive(t *testing.T) {
	got := clientIPUnder(t, "NONE", "203.0.113.9:5555", "198.51.100.7")
	if got != "203.0.113.9" {
		t.Fatalf("client IP = %q, want the transport peer", got)
	}
}
