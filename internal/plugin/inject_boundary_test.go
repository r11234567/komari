package plugin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHTMLInjectorBlocksAllPluginsOnLoginAndProtectedPaths(t *testing.T) {
	manager := &Manager{injects: []*injectEntry{
		{short: "fixture", head: `<meta name="other" content="blocked">`},
		{short: "komari-passkey", head: `<meta name="passkey" content="legacy">`},
	}}
	document := []byte("<html><head></head><body>Login</body></html>")

	// P2B removes the temporary /login compat exception: all login, verify,
	// private SPA, and control-plane routes unconditionally reject every plugin.
	for _, blocked := range []string{
		"/login",
		"/login/",
		"/login?next=%2Fadmin",
		"/login/verify",
		"/login/verify/",
		"/admin",
		"//admin",
		"/terminal",
		"/bind",
		"/bind-login",
		"/enroll",
		"/install",
		"/manage",
		"/database-recovery",
		"/api/login",
	} {
		rendered := manager.injectPublicThemeHTML(httptest.NewRequest(http.MethodGet, blocked, nil), document)
		if string(rendered) != string(document) {
			t.Fatalf("plugin must not inject into %s: %q", blocked, rendered)
		}
	}
}

func TestHTMLInjectorStillAddsOrdinaryPageMetadata(t *testing.T) {
	manager := &Manager{injects: []*injectEntry{{short: "fixture", head: `<meta name="fixture" content="enabled">`}}}
	document := []byte("<html><head></head><body>Application</body></html>")
	rendered := manager.injectPublicThemeHTML(httptest.NewRequest(http.MethodGet, "/", nil), document)
	if !strings.Contains(string(rendered), `<meta name="fixture" content="enabled"></head>`) {
		t.Fatal("normal public theme metadata injection missing")
	}
}
