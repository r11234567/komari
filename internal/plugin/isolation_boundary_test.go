package plugin

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
			"pages":[{"type":"iframe","visibility":"public","title":"Home","file":"index.html"}]
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

func TestPluginBoundaryP1IsolationAndServerHardening(t *testing.T) {
	// 1. /api/me and OAuth/2FA/temp/sign-on cookies belong strictly to the core workspace boundary.
	if !protectedWorkspacePath("/api/me") || !protectedWorkspacePath("/api/me/2fa") {
		t.Fatal("expected /api/me and subpaths to be protected workspace paths")
	}
	for _, name := range []string{"oauth_state", "oauth_return_to", "binding_external_account", "2fa_secret", "temp_key", "komari_sign_on_grant", "OAuth_State"} {
		if !isReservedSessionCookieName(name) {
			t.Fatalf("expected cookie %q to be reserved", name)
		}
	}

	// 2. disableWithError must clear grants case-insensitively for Komari-Passkey.
	withTempDataDir(t)
	gin.SetMode(gin.TestMode)
	if err := global.disableWithError("Komari-Passkey", PluginState{
		Enabled:                 true,
		ApprovedPermissionsHash: "hash",
		ApprovedPackageDigest:   "digest",
	}, errors.New("load failed")); err == nil {
		t.Fatal("expected disableWithError to return wrapped error")
	}
	if st := global.stateStore().get("Komari-Passkey"); st.Enabled || st.ApprovedPermissionsHash != "" || st.ApprovedPackageDigest != "" {
		t.Fatalf("disableWithError did not wipe Komari-Passkey grants: %+v", st)
	}

	// 3. Static root "." rejection, symlink escape blocked via os.OpenRoot, ordinary/streaming Set-Cookie stripping,
	//    and request hook protection of restricted auth/cookie/forwarding headers.
	engine := gin.New()
	engine.GET("/hook-check", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"auth":   c.GetHeader("Authorization"),
			"cookie": c.GetHeader("Cookie"),
			"xff":    c.GetHeader("X-Forwarded-For"),
			"hook":   c.GetHeader("X-Hook"),
		})
	})
	Init(engine)

	outsideDir := t.TempDir()
	outsideSecret := filepath.Join(outsideDir, "secret.txt")
	if err := os.WriteFile(outsideSecret, []byte("top-secret"), 0600); err != nil {
		t.Fatal(err)
	}

	zipPath := writePluginZip(t, map[string]string{
		"komari-plugin.json": `{"name":"Boundary","short":"bnd","version":"1.0.0","permissions":{"allowRoutes":true,"allowHooks":true}}`,
		"dist/index.html":    `<h1>static-ok</h1>`,
		"script.js": `
			const server = require("server");
			function load() {
				server.route("GET", "/plugin-cookies", (_req, res) => {
					res.setHeader("Set-Cookie", ["oauth_state=evil; Path=/", "__Host-komari_session=evil; Path=/", "custom_pref=good; Path=/"]);
					res.end("ok");
				});
				server.route("GET", "/plugin-stream-cookies", (_req, res) => {
					res.streaming = true;
					res.setHeader("Set-Cookie", ["2fa_secret=evil; Path=/", "session_token=evil; Path=/", "theme=dark; Path=/"]);
					res.write("chunk");
					res.end();
				});
				let rootRejected = false;
				try {
					server.static("/root-static", ".");
				} catch (_) {
					rootRejected = true;
				}
				server.route("GET", "/root-static-status", (_req, res) => {
					res.end(rootRejected ? "rejected" : "allowed");
				});
				server.static("/assets", "dist");
				server.hook("request", (req) => {
					req.headers = {
						"authorization": "Bearer attacker",
						"cookie": "session_token=forged",
						"x-forwarded-for": "127.0.0.1",
						"x-hook": "applied",
					};
				});
			}
		`,
	})
	if _, err := InstallZip(zipPath); err != nil {
		t.Fatal(err)
	}
	if err := SetEnabled("bnd", true, true); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideSecret, filepath.Join(DataDir, "bnd", "dist", "leak.txt")); err != nil {
		t.Fatalf("symlink setup: %v", err)
	}

	wrapped := WrapHandler(engine)

	// Confirm server.static(".") was rejected cleanly.
	recStaticDot := httptest.NewRecorder()
	wrapped.ServeHTTP(recStaticDot, httptest.NewRequest(http.MethodGet, "/root-static-status", nil))
	if recStaticDot.Code != http.StatusOK || recStaticDot.Body.String() != "rejected" {
		t.Fatalf("server.static(\".\") probe failed: code=%d body=%q", recStaticDot.Code, recStaticDot.Body.String())
	}

	// Ordinary and streaming server.route Set-Cookie headers strip reserved names.
	for _, tc := range []struct {
		path string
		keep string
	}{
		{"/plugin-cookies", "custom_pref=good; Path=/"},
		{"/plugin-stream-cookies", "theme=dark; Path=/"},
	} {
		rec := httptest.NewRecorder()
		wrapped.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d", tc.path, rec.Code)
		}
		got := rec.Header().Values("Set-Cookie")
		if len(got) != 1 || got[0] != tc.keep {
			t.Fatalf("%s Set-Cookie = %v, want [%q]", tc.path, got, tc.keep)
		}
	}

	// Static file inside dist succeeds with nosniff; symlink escape outside dist returns 404.
	recAsset := httptest.NewRecorder()
	wrapped.ServeHTTP(recAsset, httptest.NewRequest(http.MethodGet, "/assets/index.html", nil))
	if recAsset.Code != http.StatusOK || recAsset.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("static asset = %d, nosniff=%q", recAsset.Code, recAsset.Header().Get("X-Content-Type-Options"))
	}
	recLeak := httptest.NewRecorder()
	wrapped.ServeHTTP(recLeak, httptest.NewRequest(http.MethodGet, "/assets/leak.txt", nil))
	if recLeak.Code != http.StatusNotFound {
		t.Fatalf("static symlink escape status = %d, want 404", recLeak.Code)
	}

	// Request hook cannot overwrite Authorization, Cookie, or X-Forwarded-For.
	hookReq := httptest.NewRequest(http.MethodGet, "/hook-check", nil)
	hookReq.Header.Set("Authorization", "Bearer real-token")
	hookReq.Header.Set("Cookie", "session_token=real-session")
	hookReq.Header.Set("X-Forwarded-For", "203.0.113.5")
	hookRec := httptest.NewRecorder()
	wrapped.ServeHTTP(hookRec, hookReq)
	if hookRec.Code != http.StatusOK {
		t.Fatalf("hook-check status = %d", hookRec.Code)
	}
	var gotHeaders map[string]string
	if err := json.Unmarshal(hookRec.Body.Bytes(), &gotHeaders); err != nil {
		t.Fatal(err)
	}
	if gotHeaders["auth"] != "Bearer real-token" || gotHeaders["cookie"] != "session_token=real-session" || gotHeaders["xff"] != "203.0.113.5" || gotHeaders["hook"] != "applied" {
		t.Fatalf("hook mutated restricted headers: %+v", gotHeaders)
	}
}
