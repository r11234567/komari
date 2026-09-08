package security

import (
	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/pkg/rpc"
)

// PrincipalHeader carries how the request authenticated, for the benefit of a
// reverse proxy and whatever reads its access log.
//
// It exists because a log-driven IP banning layer can otherwise only guess. A
// proxy sees a URL and a status code, so distinguishing "an Agent reporting
// normally" from "a scanner probing an endpoint that happens to 404" means
// pattern-matching paths, which breaks whenever a route is added, and cannot
// separate an authenticated request from an unauthenticated one against the
// same path. Emitting the answer directly turns that guess into a fact the
// proxy can log and filter on.
//
// Deliberately coarse: it names the credential class, never which account. A
// value here must not let anyone reading proxy logs enumerate users or agents.
const PrincipalHeader = "X-Komari-Principal"

// Principal classes reported through PrincipalHeader.
const (
	// PrincipalHeaderAgent is a request bearing a valid Agent client token.
	PrincipalHeaderAgent = "agent"
	// PrincipalHeaderUser is a request bearing a valid admin session cookie.
	PrincipalHeaderUser = "user"
	// PrincipalHeaderAPIKey is a request bearing the configured API key.
	PrincipalHeaderAPIKey = "api-key"
	// PrincipalHeaderAnonymous is a request that presented no accepted
	// credential. Note this covers both a caller that offered none and one
	// whose credential was rejected: neither is trusted, and telling them apart
	// in a proxy log would leak whether a token or account exists.
	PrincipalHeaderAnonymous = "anonymous"
)

// PrincipalClass maps an identified principal to its header value.
func PrincipalClass(p *rpc.Principal) string {
	if p == nil {
		return PrincipalHeaderAnonymous
	}
	switch p.Type {
	case rpc.PrincipalAgent:
		return PrincipalHeaderAgent
	case rpc.PrincipalUser:
		return PrincipalHeaderUser
	case rpc.PrincipalAPIKey:
		return PrincipalHeaderAPIKey
	default:
		return PrincipalHeaderAnonymous
	}
}

// SetPrincipalHeader records the caller's credential class on the response.
//
// It is set before the handler runs so it is present on error responses too -
// which is the case that matters, since those are exactly the responses a
// banning layer counts.
func SetPrincipalHeader(c *gin.Context, p *rpc.Principal) {
	c.Header(PrincipalHeader, PrincipalClass(p))
}
