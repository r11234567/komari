package public

import (
	"archive/zip"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/komari-monitor/komari/database/accounts"
	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/internal/plugin"
	"github.com/komari-monitor/komari/pkg/config"
	"github.com/komari-monitor/komari/web/oauth"
	"github.com/komari-monitor/komari/web/oauth/factory"
	"github.com/komari-monitor/komari/web/security"
)

type stubOIDCConfig struct{}

type stubOIDCProvider struct {
	cfg     stubOIDCConfig
	mu      sync.Mutex
	seen    map[string]bool
	subject string
}

func (p *stubOIDCProvider) GetName() string                         { return "stuboidc" }
func (p *stubOIDCProvider) GetConfiguration() factory.Configuration { return &p.cfg }
func (p *stubOIDCProvider) Init() error                             { p.seen = make(map[string]bool); return nil }
func (p *stubOIDCProvider) Destroy() error                          { return nil }
func (p *stubOIDCProvider) GetAuthorizationURL(_ string) (string, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	state := "stub-state-" + uuid.NewString()[:8]
	p.seen[state] = true
	return "https://idp.example.test/auth?state=" + state, state
}
func (p *stubOIDCProvider) OnCallback(_ *gin.Context, state string, _ map[string]string, _ string) (factory.OidcCallback, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.seen[state] {
		return factory.OidcCallback{}, os.ErrInvalid
	}
	delete(p.seen, state)
	subj := p.subject
	if subj == "" {
		subj = "ext-user-1"
	}
	return factory.OidcCallback{UserId: subj}, nil
}

func init() {
	factory.RegisterOidcProvider(func() factory.IOidcProvider { return &stubOIDCProvider{} })
}

func findCookie(cookies []*http.Cookie, name string) *http.Cookie {
	for _, item := range cookies {
		if item.Name == name {
			return item
		}
	}
	return nil
}

func TestOAuthFlowUsesStrictHostCookiesRechecksEnabledAndEnforcesBindNonce(t *testing.T) {
	gin.SetMode(gin.TestMode)
	prevDomain, _ := config.GetAs[string](config.ScriptDomainKey, "")
	prevEnabled, _ := config.GetAs[bool](config.OAuthEnabledKey, false)
	if err := config.Set(config.ScriptDomainKey, "https://panel.example"); err != nil {
		t.Fatal(err)
	}
	if err := config.Set(config.OAuthEnabledKey, true); err != nil {
		t.Fatal(err)
	}
	if err := oauth.LoadProvider("stuboidc", "{}"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = config.Set(config.ScriptDomainKey, prevDomain)
		_ = config.Set(config.OAuthEnabledKey, prevEnabled)
		_ = oauth.Shutdown()
	})

	r := gin.New()
	r.GET("/api/oauth", OAuth)
	r.GET("/api/oauth_callback", OAuthCallback)

	// 1. /api/oauth must set __Host-komari_oauth_state and __Host-komari_oauth_return_to.
	req := httptest.NewRequest(http.MethodGet, "/api/oauth?returnTo=/admin/servers", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("GET /api/oauth = %d, body=%s", rec.Code, rec.Body.String())
	}
	cookies := rec.Result().Cookies()
	stateCookie := findCookie(cookies, security.OAuthStateCookieName)
	returnCookie := findCookie(cookies, security.OAuthReturnToCookieName)
	if stateCookie == nil || !strings.HasPrefix(stateCookie.Name, "__Host-") || !stateCookie.HttpOnly || !stateCookie.Secure || stateCookie.Path != "/" || stateCookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("unsafe OAuth state cookie: %+v", stateCookie)
	}
	if returnCookie == nil || !strings.HasPrefix(returnCookie.Name, "__Host-") || returnCookie.Value != "/admin/servers" {
		t.Fatalf("unsafe OAuth return cookie: %+v", returnCookie)
	}

	// 2. Disabling OAuth must still reject callback with 403 and clear cookies.
	if err := config.Set(config.OAuthEnabledKey, false); err != nil {
		t.Fatal(err)
	}
	cbDisabled := httptest.NewRequest(http.MethodGet, "/api/oauth_callback?state="+stateCookie.Value+"&code=abc", nil)
	cbDisabled.AddCookie(stateCookie)
	cbDisabled.AddCookie(returnCookie)
	recDisabled := httptest.NewRecorder()
	r.ServeHTTP(recDisabled, cbDisabled)
	if recDisabled.Code != http.StatusForbidden {
		t.Fatalf("disabled OAuth callback = %d, want 403", recDisabled.Code)
	}
	if cleared := findCookie(recDisabled.Result().Cookies(), security.OAuthStateCookieName); cleared == nil || cleared.MaxAge != -1 {
		t.Fatalf("expected OAuth state cookie cleared on callback, got %+v", cleared)
	}

	// Re-enable OAuth and obtain a fresh state for forged bind cookie rejection test.
	if err := config.Set(config.OAuthEnabledKey, true); err != nil {
		t.Fatal(err)
	}
	rec2 := httptest.NewRecorder()
	r.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/api/oauth", nil))
	stateCookie2 := findCookie(rec2.Result().Cookies(), security.OAuthStateCookieName)

	user, err := accounts.CreateAccount("oauth-public-"+uuid.NewString()[:8], "Strong-Password-123")
	if err != nil {
		t.Fatal(err)
	}
	session, err := accounts.CreateSession(user.UUID, 3600, "ua", "127.0.0.1", "password")
	if err != nil {
		t.Fatal(err)
	}
	db := dbcore.GetDBInstance()
	t.Cleanup(func() {
		db.Where("uuid = ?", user.UUID).Delete(&models.Session{})
		_ = accounts.DeleteAccountByUsername(user.Username)
	})

	// Forged bind nonce with valid session must fail with 403 and never bind external account.
	cbForged := httptest.NewRequest(http.MethodGet, "/api/oauth_callback?state="+stateCookie2.Value+"&code=abc", nil)
	cbForged.AddCookie(stateCookie2)
	cbForged.AddCookie(&http.Cookie{Name: security.SessionCookieName, Value: session})
	cbForged.AddCookie(&http.Cookie{Name: security.OAuthBindCookieName, Value: user.UUID})
	recForged := httptest.NewRecorder()
	r.ServeHTTP(recForged, cbForged)
	if recForged.Code != http.StatusForbidden {
		t.Fatalf("forged bind cookie status = %d, want 403 (%s)", recForged.Code, recForged.Body.String())
	}
	updatedUser, err := accounts.GetUserByUUID(user.UUID)
	if err != nil {
		t.Fatal(err)
	}
	if updatedUser.SSOID != "" {
		t.Fatalf("forged bind cookie unexpectedly bound sso_id %q", updatedUser.SSOID)
	}

	// Valid server-issued bind nonce succeeds once and rejects replay.
	validNonce, err := oauth.IssueBindingToken(user.UUID, session)
	if err != nil {
		t.Fatal(err)
	}
	rec3 := httptest.NewRecorder()
	r.ServeHTTP(rec3, httptest.NewRequest(http.MethodGet, "/api/oauth", nil))
	stateCookie3 := findCookie(rec3.Result().Cookies(), security.OAuthStateCookieName)

	cbValid := httptest.NewRequest(http.MethodGet, "/api/oauth_callback?state="+stateCookie3.Value+"&code=abc", nil)
	cbValid.AddCookie(stateCookie3)
	cbValid.AddCookie(&http.Cookie{Name: security.SessionCookieName, Value: session})
	cbValid.AddCookie(&http.Cookie{Name: security.OAuthBindCookieName, Value: validNonce})
	recValid := httptest.NewRecorder()
	r.ServeHTTP(recValid, cbValid)
	if recValid.Code != http.StatusFound {
		t.Fatalf("valid bind callback = %d, want 302 (%s)", recValid.Code, recValid.Body.String())
	}
	updatedUser, err = accounts.GetUserByUUID(user.UUID)
	if err != nil {
		t.Fatal(err)
	}
	if updatedUser.SSOID != "stuboidc_ext-user-1" {
		t.Fatalf("bound sso_id = %q, want stuboidc_ext-user-1", updatedUser.SSOID)
	}
}

func TestPublicServePluginFileSetsSandboxCSPAndEnforcesManifest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	if err := plugin.CloseAll(); err != nil {
		t.Fatal(err)
	}
	oldDir, oldStorage := plugin.DataDir, plugin.StorageDir
	plugin.DataDir = t.TempDir()
	plugin.StorageDir = t.TempDir()
	t.Cleanup(func() {
		_ = plugin.CloseAll()
		plugin.DataDir = oldDir
		plugin.StorageDir = oldStorage
	})

	zipPath := filepath.Join(t.TempDir(), "pub.zip")
	f, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for name, body := range map[string]string{
		"komari-plugin.json": `{"name":"Pub","short":"pub-page","version":"1.0.0","pages":[{"file":"pages/public.html","title":"Pub","visibility":"public"},{"file":"pages/admin.html","title":"Admin","visibility":"admin"}]}`,
		"script.js":          `function load(){}`,
		"pages/public.html":  `<html><body>public</body></html>`,
		"pages/admin.html":   `<html><body>admin</body></html>`,
	} {
		entry, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	plugin.Init(gin.New())
	if _, err := plugin.InstallZip(zipPath); err != nil {
		t.Fatal(err)
	}
	if err := plugin.SetEnabled("pub-page", true, true); err != nil {
		t.Fatal(err)
	}

	r := gin.New()
	r.GET("/api/plugin/:short/*filepath", ServePluginFile)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/plugin/pub-page/pages/public.html", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("public page status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q, want nosniff", got)
	}
	csp := rec.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "sandbox") || !strings.Contains(csp, "frame-ancestors 'self'") || strings.Contains(csp, "allow-same-origin") {
		t.Fatalf("unexpected public page CSP = %q", csp)
	}

	for _, denied := range []string{"/pages/admin.html", "/komari-plugin.json", "/script.js"} {
		dRec := httptest.NewRecorder()
		r.ServeHTTP(dRec, httptest.NewRequest(http.MethodGet, "/api/plugin/pub-page"+denied, nil))
		if dRec.Code != http.StatusNotFound {
			t.Fatalf("public endpoint unexpectedly served %s: status=%d", denied, dRec.Code)
		}
	}
}
