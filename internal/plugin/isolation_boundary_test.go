package plugin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/web/security"
)

func TestPluginBoundaryHeadersCookiesRoutesAndLegacyPasskey(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/plugin-page?ok=1", nil)
	req.Header.Set("Cookie", security.SessionCookieName+"=secret-session; theme=dark")
	req.Header.Set("Authorization", "Bearer komari_pat_secret123")
	req.Header.Set("X-2FA-Code", "123456")
	req.Header.Set("X-Two-Factor-Code", "654321")
	req.Header.Set("X-Komari-Workspace-Grant", "grant-secret")
	req.Header.Set("X-Custom-Plugin-Header", "allowed")

	hookSanitized := sanitizedPluginHeaders(req.Header, false)
	for _, stripped := range []string{"Cookie", "Authorization", "X-2FA-Code", "X-Two-Factor-Code", "X-Komari-Workspace-Grant"} {
		if hookSanitized.Get(stripped) != "" {
			t.Fatalf("hook header %s leaked into plugin runtime", stripped)
		}
	}
	if hookSanitized.Get("X-Custom-Plugin-Header") != "allowed" {
		t.Fatalf("expected non-sensitive header preserved in hook")
	}

	routeSanitized := sanitizedPluginHeaders(req.Header, true)
	if routeSanitized.Get("Authorization") != "" {
		t.Fatalf("Komari PAT Authorization leaked into plugin-owned HTTP route")
	}
	foreignReq := httptest.NewRequest(http.MethodGet, "/plugin-route", nil)
	foreignReq.Header.Set("Authorization", "Bearer external-webhook-token")
	foreignReq.Header.Set("Cookie", "session_token=secret")
	foreignSanitized := sanitizedPluginHeaders(foreignReq.Header, true)
	if foreignSanitized.Get("Authorization") != "Bearer external-webhook-token" {
		t.Fatalf("expected foreign plugin Bearer token preserved on plugin route")
	}
	if foreignSanitized.Get("Cookie") != "" {
		t.Fatalf("expected Cookie stripped on plugin route")
	}

	respHeaders := http.Header{}
	respHeaders.Add("Set-Cookie", security.SessionCookieName+"=forged; Path=/")
	respHeaders.Add("Set-Cookie", "session_token=forged; Path=/")
	respHeaders.Add("Set-Cookie", "komari_temp=forged; Path=/")
	respHeaders.Add("Set-Cookie", "plugin_pref=ok; Path=/")
	stripReservedSetCookies(respHeaders)
	remaining := respHeaders.Values("Set-Cookie")
	if len(remaining) != 1 || !strings.HasPrefix(remaining[0], "plugin_pref=ok") {
		t.Fatalf("reserved session Set-Cookie survived sanitization: %v", remaining)
	}

	for _, protected := range []string{
		"/api/bind/ticket",
		"/api/public/bind/ticket",
		"/api/clients/report",
		"/api/clients/terminal",
		"/api/clients/remote",
		"/api/admin/settings",
		"/komari.browser.v1.BrowserService/GetSession",
		"/login",
		"/admin",
	} {
		if !protectedWorkspacePath(protected) {
			t.Fatalf("protected path %q was not blocked from plugin surface", protected)
		}
	}
	if protectedWorkspacePath("/api/clients") {
		t.Fatal("public readonly /api/clients websocket path should stay hook-compatible")
	}

	withTempDataDir(t)
	gin.SetMode(gin.TestMode)
	Init(gin.New())

	reservedRouteZip := writePluginZip(t, map[string]string{
		"komari-plugin.json": `{"name":"RouteTest","short":"routetest","version":"1.0.0","permissions":{"allowRoutes":true,"timeout":5}}`,
		"script.js": `
			const server = require("server");
			function load() {
				server.route("GET", "/api/clients/report", (req, res) => { res.send("pwn"); });
			}
		`,
	})
	if _, err := InstallZip(reservedRouteZip); err != nil {
		t.Fatal(err)
	}
	if err := SetEnabled("routetest", true, true); err == nil {
		t.Fatal("expected server.route on /api/clients/report to fail")
	}

	assetZip := writePluginZip(t, map[string]string{
		"komari-plugin.json": `{
			"name":"Assets",
			"short":"assets",
			"version":"1.0.0",
			"entry":"App.js",
			"pages":[{"type":"iframe","visibility":"public","file":"index.html"}]
		}`,
		"App.js":           "function load() {}",
		"index.html":       "<html>ok</html>",
		"style.css":        "body{}",
		"admin/panel.html": "<html>secret-admin</html>",
	})
	if _, err := InstallZip(assetZip); err != nil {
		t.Fatal(err)
	}
	if err := SetEnabled("assets", true, true); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = SetEnabled("assets", false, false) }()

	if err := validatePublicFile("assets", "style.css"); err != nil {
		t.Fatalf("root public CSS unexpectedly blocked: %v", err)
	}
	if err := validatePublicFile("assets", "admin/panel.html"); err == nil {
		t.Fatal("subdirectory admin/panel.html leaked under dir == '.' rule")
	}
	if err := validatePublicFile("assets", "app.js"); err == nil {
		t.Fatal("case-variant entrypoint app.js leaked publicly")
	}

	legacyZip := writePluginZip(t, map[string]string{
		"komari-plugin.json": `{"name":"Legacy","short":"Komari-Passkey","version":"1.0.0"}`,
		"script.js":          "function load() {}",
	})
	if _, err := InstallZip(legacyZip); err != nil {
		t.Fatal(err)
	}
	if err := SetEnabled("Komari-Passkey", true, true); err == nil {
		t.Fatal("case-variant Komari-Passkey was enabled")
	}
	_ = json.Valid([]byte("{}"))
}
