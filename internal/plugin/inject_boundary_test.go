package plugin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHTMLInjectorLeavesFileDataPlaneUnwrapped(t *testing.T) {
	manager := &Manager{injects: []*injectEntry{{short: "fixture", head: `<meta name="fixture" content="enabled">`}}}
	handler := manager.htmlInjectHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, wrapped := w.(*htmlInjectWriter); wrapped {
			t.Error("file data plane must receive the original writer")
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte("ordinary file contents"))
	}))
	for _, path := range []string{
		"/api/admin/client/fixture/file/download",
		"/api/preview/client/fixture/file/download",
		"/api/clients/transfer/fixture",
		"/komari.file.v1.FileService/ListDirectory",
		"/komari.credential.v1.CredentialService/Begin",
		"/admin",
		"/admin/settings",
		"/terminal",
		"/bind",
		"/login",
		"/api/admin/settings",
		"/api/login",
		"/api/oauth",
	} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Body.String() != "ordinary file contents" {
			t.Fatalf("protected path %q was modified", path)
		}
	}
}

func TestHTMLInjectorBlocksAllPluginsOnLoginAndProtectedPaths(t *testing.T) {
	manager := &Manager{injects: []*injectEntry{
		{short: "fixture", head: `<meta name="other" content="blocked">`},
		{short: "komari-passkey", head: `<meta name="passkey" content="legacy">`},
	}}
	handler := manager.htmlInjectHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<html><head></head><body>Login</body></html>"))
	}))

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
		recBlocked := httptest.NewRecorder()
		handler.ServeHTTP(recBlocked, httptest.NewRequest(http.MethodGet, blocked, nil))
		if strings.Contains(recBlocked.Body.String(), `name="passkey"`) || strings.Contains(recBlocked.Body.String(), `name="other"`) {
			t.Fatalf("plugin must not inject into %s: %q", blocked, recBlocked.Body.String())
		}
	}
}

func TestHTMLInjectorStillAddsOrdinaryPageMetadata(t *testing.T) {
	manager := &Manager{injects: []*injectEntry{{short: "fixture", head: `<meta name="fixture" content="enabled">`}}}
	handler := manager.htmlInjectHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<html><head></head><body>Application</body></html>"))
	}))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if !strings.Contains(recorder.Body.String(), `<meta name="fixture" content="enabled"></head>`) {
		t.Fatal("normal page metadata injection missing")
	}
	if recorder.Header().Get("Content-Security-Policy") != "" {
		t.Fatal("injector changed the application's content policy")
	}
}
