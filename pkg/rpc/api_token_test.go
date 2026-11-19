package rpc

import (
	"testing"
	"time"
)

func TestScopedAPITokenCannotAcquireUndeclaredOrInteractivePermissions(t *testing.T) {
	p := &Principal{Type: PrincipalAPIKey, IsAPIKey: true, Roles: []string{RoleGuest}, APIToken: &APITokenPolicy{ID: "test", Scopes: []string{ScopeNodesRead}, AgentIDs: []string{"allowed"}, ExpiresAt: time.Now().Add(time.Hour)}}
	if !p.AllowsAPIProcedure("/komari.browser.v1.BrowserService/GetAgent") || !p.AllowsAgent("allowed") {
		t.Fatal("explicit scope rejected")
	}
	for _, path := range []string{"/komari.metrics.v1.MetricsService/QueryMetrics", "/komari.admin.v1.ApiTokenService/CreateApiToken", "/komari.webssh.v1.WebSSHService/CreateSession", "/komari.exec.v1.ExecutionService/CreateExecution", "/komari.file.v1.FileService/ListDirectory", "/api/admin/settings", "/rpc2", "/future.Service/GetEverything"} {
		if p.AllowsAPIProcedure(path) {
			t.Fatalf("unreviewed procedure accepted: %s", path)
		}
	}
	if p.AllowsAgent("other") {
		t.Fatal("cross-node permission leak")
	}
	for _, method := range []string{"admin:exec", "admin:editClient", "public:getNodes"} {
		if CheckPrincipal(p, method) {
			t.Fatalf("legacy role fallback accepted %s", method)
		}
	}
	p.APIToken.ExpiresAt = time.Now().Add(-time.Second)
	if p.AllowsAgent("allowed") || p.AllowsAPIProcedure("/komari.browser.v1.BrowserService/GetAgent") {
		t.Fatal("expired token accepted")
	}
}
