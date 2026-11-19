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
	if err := app.BuildRouter(); err != nil {
		t.Fatal(err)
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
