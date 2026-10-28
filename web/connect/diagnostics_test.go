package connectapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/internal/diagnostics"
	logger "github.com/komari-monitor/komari/utils/log"
	"github.com/komari-monitor/komari/web/api"
	"github.com/komari-monitor/komari/web/api/admin"
	credentialv1 "github.com/r11234567/komari-proto/gen/go/komari/credential/v1"
	credentialv1connect "github.com/r11234567/komari-proto/gen/go/komari/credential/v1/credentialv1connect"
)

func TestDiagnosticsRealConnectAuthenticationAndRecovery(t *testing.T) {
	// This is the same registry, middleware, recovery and generated handlers as
	// production, backed by the package's real temporary SQLite fixtures.
	r := gin.New()
	r.Use(diagnostics.Default.Middleware(), logger.GinRecovery())
	Register(r)
	r.GET("/synthetic-panic", func(*gin.Context) { panic("synthetic diagnostic panic") })
	server := httptest.NewServer(r)
	defer server.Close()
	client := credentialv1connect.NewTrustedCredentialServiceClient(server.Client(), server.URL)
	path := "/komari.credential.v1.TrustedCredentialService/ListTrustedCredentialProviders"
	label := "CONNECT " + path + " unary"
	read := func() diagnostics.RouteSnapshot {
		for _, v := range diagnostics.Default.Snapshot().Routes {
			if v.Route == label {
				return v
			}
		}
		return diagnostics.RouteSnapshot{HTTPStatusClasses: make([]int64, 6)}
	}
	before := read()
	if _, err := client.ListTrustedCredentialProviders(context.Background(), connect.NewRequest(&credentialv1.ListTrustedCredentialProvidersRequest{})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("anonymous access changed: %v", err)
	}
	_, _, session := credentialTestIdentity(t)
	req := connect.NewRequest(&credentialv1.ListTrustedCredentialProvidersRequest{})
	req.Header().Set("Cookie", "session_token="+session)
	if _, err := client.ListTrustedCredentialProviders(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	badReq, _ := http.NewRequest("POST", server.URL+path, strings.NewReader("{}"))
	badReq.Header.Set("Content-Type", "application/json")
	badReq.Header.Set("Authorization", "Bearer synthetic-invalid-token")
	badReq.Header.Set("X-Komari-Signature", "synthetic-invalid-signature")
	badResp, err := server.Client().Do(badReq)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, badResp.Body)
	badResp.Body.Close()
	if badResp.StatusCode != 401 {
		t.Fatalf("DPoP rejection changed: %d", badResp.StatusCode)
	}
	resp, err := server.Client().Get(server.URL + "/synthetic-panic")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 500 {
		t.Fatal("panic recovery changed")
	}
	server.Close()
	after := read()
	if after.Completed-before.Completed != 3 || after.RPCCodes["permission_denied"]-before.RPCCodes["permission_denied"] != 1 || after.RPCCodes["ok"]-before.RPCCodes["ok"] != 1 || after.HTTPStatusClasses[4]-before.HTTPStatusClasses[4] != 2 {
		t.Fatalf("real generated Connect outcomes missing: before=%+v after=%+v", before, after)
	}
	found := false
	for _, v := range diagnostics.Default.Snapshot().Routes {
		if v.Route == "GET /synthetic-panic" {
			found = v.Panics == 1 && v.HTTPStatusClasses[5] == 1 && v.InFlight == 0
		}
	}
	if !found {
		t.Fatal("recovered panic not measured")
	}
	raw, _ := json.Marshal(diagnostics.Default.Snapshot())
	if strings.Contains(string(raw), session) {
		t.Fatal("session leaked")
	}
}

func TestDiagnosticsAdminEndpointAuthorizationAndNoSQL(t *testing.T) {
	r := gin.New()
	r.Use(api.IdentityMiddleware())
	admin.RegisterPprofRoutes(r.Group("/api/admin", api.RequireRole(api.RoleAdmin)))
	server := httptest.NewServer(r)
	defer server.Close()
	resp, err := server.Client().Get(server.URL + "/api/admin/diagnostics")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous diagnostics exposed: %d", resp.StatusCode)
	}
	// Verify auth with a real cookie before testing handler behavior under pool
	// contention. Authentication itself still requires the database; it is not
	// bypassed or cached by the diagnostics feature.
	_, _, session := credentialTestIdentity(t)
	req, _ := http.NewRequest("GET", server.URL+"/api/admin/diagnostics", nil)
	req.AddCookie(&http.Cookie{Name: "session_token", Value: session})
	resp, err = server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("authenticated response: %d %s", resp.StatusCode, raw)
	}
	var body struct{ Data map[string]json.RawMessage }
	if err = json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"requests", "database", "runtime", "report_queue", "workspace", "verification"} {
		if len(body.Data[key]) == 0 {
			t.Fatalf("missing real diagnostic source %s", key)
		}
	}
	if strings.Contains(string(raw), session) || strings.Contains(string(raw), testAccessToken) {
		t.Fatal("credentials leaked")
	}
	pool, err := dbcore.GetDBInstance().DB()
	if err != nil {
		t.Fatal(err)
	}
	held, err := pool.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	// Test the real handler in isolation after independently checking its actual
	// authentication boundary above. Production keeps that boundary intact.
	fixture := gin.New()
	admin.RegisterPprofRoutes(fixture.Group("/api/admin"))
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		fixture.ServeHTTP(w, httptest.NewRequest("GET", "/api/admin/diagnostics", nil))
		done <- w
	}()
	select {
	case w := <-done:
		if w.Code != 200 {
			t.Fatalf("snapshot unavailable under pool contention: %d", w.Code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("diagnostics handler blocked acquiring DB connection")
	}
}
