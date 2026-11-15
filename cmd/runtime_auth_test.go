package cmd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/komari-monitor/komari/database/apitokens"
	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/pkg/config"
	"github.com/komari-monitor/komari/pkg/rpc"
)

func TestProductionRouterEnforcesScopedTokenAuthentication(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("KOMARI_TRUSTED_PROXIES", "none")
	app := NewApp()
	if err := app.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	defer app.Shutdown()
	if err := config.Set(config.CorsAllowedOriginsKey, "https://console.example"); err != nil {
		t.Fatal(err)
	}
	app.settings.CorsOriginCheckEnabled = true
	app.settings.CorsAllowedOrigins = "https://console.example"
	if err := app.BuildRouter(); err != nil {
		t.Fatal(err)
	}
	procedure := "/komari.browser.v1.BrowserService/ListAgents"
	preflight := httptest.NewRequest(http.MethodOptions, "http://example.com"+procedure, nil)
	preflight.RemoteAddr = "198.51.100.9:12345"
	preflight.Header.Set("Origin", "https://console.example")
	preflight.Header.Set("Access-Control-Request-Method", "POST")
	preflight.Header.Set("Access-Control-Request-Headers", "authorization,content-type,connect-protocol-version,x-komari-workspace-grant")
	// Even an invalid supplied bearer must not spend the preflight budget.
	preflight.Header.Set("Authorization", "Bearer invalid-preflight-credential")
	for i := 0; i < 15; i++ {
		w := httptest.NewRecorder()
		app.engine.ServeHTTP(w, preflight.Clone(context.Background()))
		if w.Code != 204 {
			t.Fatalf("preflight reached authentication: %d", w.Code)
		}
	}
	for i := 0; i < 11; i++ {
		r := httptest.NewRequest(http.MethodPost, "http://example.com"+procedure, strings.NewReader("{}"))
		r.RemoteAddr = "198.51.100.9:12345"
		r.Header.Set("Origin", "https://console.example")
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Connect-Protocol-Version", "1")
		r.Header.Set("Authorization", "Bearer invalid-real-credential")
		w := httptest.NewRecorder()
		app.engine.ServeHTTP(w, r)
		want := 401
		if i == 10 {
			want = 429
		}
		if w.Code != want || w.Header().Get("Access-Control-Allow-Origin") != "https://console.example" || !strings.Contains(w.Header().Get("Cache-Control"), "no-store") {
			t.Fatalf("unreadable rejection %d: %d %v", i, w.Code, w.Header())
		}
		if i == 10 && (w.Header().Get("Retry-After") == "" || !strings.Contains(w.Header().Get("Access-Control-Expose-Headers"), "Retry-After") || !strings.Contains(w.Body.String(), "resource_exhausted")) {
			t.Fatal("rate limit lost RPC code or browser retry metadata")
		}
	}
	owner := "router-token-owner"
	if err := dbcore.GetDBInstance().Create(&models.User{UUID: owner, Username: owner}).Error; err != nil {
		t.Fatal(err)
	}
	row, secret, err := apitokens.Create(context.Background(), owner, apitokens.Policy{Name: "router-reader", AllAgents: true, Scopes: []string{rpc.ScopeNodesRead}, ExpiresAt: time.Now().Add(time.Hour)}, "")
	if err != nil {
		t.Fatal(err)
	}
	invoke := func(secret string) int {
		request := httptest.NewRequest(http.MethodPost, "/komari.browser.v1.BrowserService/ListAgents", strings.NewReader("{}"))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Connect-Protocol-Version", "1")
		request.Header.Set("Authorization", "Bearer "+secret)
		response := httptest.NewRecorder()
		app.engine.ServeHTTP(response, request)
		return response.Code
	}
	if got := invoke(secret); got != http.StatusOK {
		t.Fatalf("valid token failed through production router: %d", got)
	}
	if err := apitokens.Revoke(context.Background(), owner, row.ID); err != nil {
		t.Fatal(err)
	}
	if got := invoke(secret); got != http.StatusUnauthorized {
		t.Fatalf("revoked token fell back to public access: %d", got)
	}
}
