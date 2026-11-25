package router

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/komari-monitor/komari/cmd/flags"
	"github.com/komari-monitor/komari/database/accounts"
	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/metricstore"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/pkg/config"
	"github.com/komari-monitor/komari/web/api"
	"github.com/komari-monitor/komari/web/security"
	"github.com/pquerna/otp/totp"
)

func TestHealthRouteDisablesCaching(t *testing.T) {
	engine := gin.New()
	registerHealthRoute(engine)
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/ping", nil))
	if response.Code != http.StatusOK || response.Body.String() != "pong" || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("ping returned %d %q with cache policy %q", response.Code, response.Body.String(), response.Header().Get("Cache-Control"))
	}
}

func TestLegacyAgentRoutesRejectEnrolledBearerWithoutBreakingTransferTickets(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := dbcore.GetDBInstance()
	agentID := uuid.NewString()
	token := uuid.NewString()
	credential := models.AgentCredential{
		Client: agentID, AgentPublicKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
		AccessTokenHash: fmt.Sprintf("%x", sha256.Sum256([]byte(token))),
		AccessExpiresAt: time.Now().Add(time.Hour),
	}
	if err := db.Create(&credential).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Delete(&credential) })

	engine := gin.New()
	engine.Use(api.APIAuthenticationMiddleware(), api.IdentityMiddleware())
	registerAgentRoutes(engine)
	for _, route := range []struct{ method, path string }{
		{http.MethodGet, "/api/clients/report"},
		{http.MethodPost, "/api/clients/report"},
		{http.MethodPost, "/api/clients/uploadBasicInfo"},
		{http.MethodPost, "/api/clients/task/result"},
		{http.MethodGet, "/api/clients/ping/tasks"},
		{http.MethodPost, "/api/clients/ping/result"},
		{http.MethodPost, "/api/clients/transfer/unissued"},
	} {
		req := httptest.NewRequest(route.method, route.path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		res := httptest.NewRecorder()
		engine.ServeHTTP(res, req)
		want := http.StatusForbidden
		if strings.Contains(route.path, "/transfer/") {
			want = http.StatusUnauthorized
		}
		if res.Code != want {
			t.Errorf("%s %s = %d, want %d: %s", route.method, route.path, res.Code, want, res.Body)
		}
	}
}

func TestRegisterDoesNotExposeHTTPSCertificateUpload(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	Register(engine)

	for _, route := range engine.Routes() {
		if route.Path == "/api/admin/settings/https/upload" {
			t.Fatalf("removed HTTPS certificate upload route is still registered: %s %s", route.Method, route.Path)
		}
	}
}

// legacyRemoteCompatRoutes are the pre-Connect terminal and remote control
// entry points. Connect WebSSH replaces all of them in production.
var legacyRemoteCompatRoutes = []string{
	"/api/clients/terminal",
	"/api/clients/remote",
	"/api/admin/client/remote",
	"/api/admin/client/remote/authorize",
	"/api/admin/client/remote/session",
	"/api/admin/client/remote/session/cancel",
	"/api/admin/client/:uuid/terminal",
}

func registeredPaths(t *testing.T) map[string]struct{} {
	t.Helper()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	Register(engine)
	paths := make(map[string]struct{})
	for _, route := range engine.Routes() {
		paths[route.Path] = struct{}{}
	}
	return paths
}

// TestDashboardRestBridgeIsRemoved keeps the dashboard on
// komari.admin.v1.DashboardService. The admin:getDashboard* RPC2 methods stay
// registered for unconverted themes, but must not regain a REST bridge.
func TestDashboardRestBridgeIsRemoved(t *testing.T) {
	paths := registeredPaths(t)
	for _, path := range []string{
		"/api/admin/dashboard",
		"/api/admin/dashboard/charts",
		"/api/admin/dashboard/alerts",
	} {
		if _, found := paths[path]; found {
			t.Fatalf("dashboard REST bridge %s is registered again", path)
		}
	}
}

// TestMaintenanceRestBridgeIsRemoved keeps sessions, audit logs, the clipboard,
// record retention and database maintenance on
// komari.admin.v1.MaintenanceService.
func TestMaintenanceRestBridgeIsRemoved(t *testing.T) {
	paths := registeredPaths(t)
	for _, path := range []string{
		"/api/admin/session/get",
		"/api/admin/session/remove",
		"/api/admin/session/remove/all",
		"/api/admin/logs",
		"/api/admin/clipboard",
		"/api/admin/clipboard/:id",
		"/api/admin/clipboard/remove",
		"/api/admin/clipboard/:id/remove",
		"/api/admin/record/clear",
		"/api/admin/record/clear/all",
		"/api/admin/database/size",
		"/api/admin/database/vacuum",
	} {
		if _, found := paths[path]; found {
			t.Fatalf("maintenance REST bridge %s is registered again", path)
		}
	}
}

// TestMigratedAdminRestBridgesAreRemoved covers the admin domains whose REST
// bridges were retired in favour of a Connect service: remote execution moved
// to komari.exec.v1.ExecutionService and probe tasks to
// komari.admin.v1.PingTaskService.
func TestMigratedAdminRestBridgesAreRemoved(t *testing.T) {
	paths := registeredPaths(t)
	for _, path := range []string{
		"/api/admin/task/all",
		"/api/admin/task/exec",
		"/api/admin/task/:task_id",
		"/api/admin/task/:task_id/result",
		"/api/admin/task/:task_id/result/:uuid",
		"/api/admin/task/client/:uuid",
		"/api/admin/ping/",
		"/api/admin/ping/add",
		"/api/admin/ping/delete",
		"/api/admin/ping/edit",
		"/api/admin/ping/order",
	} {
		if _, found := paths[path]; found {
			t.Fatalf("migrated REST bridge %s is registered again", path)
		}
	}
}

func TestLegacyRemoteRoutesAreUnmountedByDefault(t *testing.T) {
	paths := registeredPaths(t)
	for _, path := range legacyRemoteCompatRoutes {
		if _, found := paths[path]; found {
			t.Fatalf("legacy remote route %s must stay unmounted without KOMARI_LEGACY_REMOTE_COMPAT", path)
		}
	}
}

func TestLegacyRemoteRoutesMountWhenCompatEnabled(t *testing.T) {
	t.Setenv("KOMARI_LEGACY_REMOTE_COMPAT", "true")
	paths := registeredPaths(t)
	for _, path := range legacyRemoteCompatRoutes {
		if _, found := paths[path]; !found {
			t.Fatalf("legacy remote route %s must mount when KOMARI_LEGACY_REMOTE_COMPAT is enabled", path)
		}
	}
}

func TestSplitPublicAndPanelListenersEnforceHardBoundaries(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("KOMARI_PANEL_LISTEN", "127.0.0.1:25775")
	t.Setenv("KOMARI_PANEL_ORIGIN", "https://panel.example.com")
	t.Setenv("KOMARI_PUBLIC_ORIGIN", "https://status.example.com")
	flags.PanelListen = ""
	flags.PanelOrigin = ""
	flags.PublicOrigin = ""

	db := dbcore.GetDBInstance()
	if err := metricstore.InitializeStore(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = metricstore.CloseStoreContext(t.Context())
	})
	user, err := accounts.CreateAccount("split-admin-"+uuid.NewString()[:8], "Strong-Password-123")
	if err != nil {
		t.Fatal(err)
	}
	session, err := accounts.CreateSession(user.UUID, 3600, "test-agent", "127.0.0.1", "password")
	if err != nil {
		t.Fatal(err)
	}
	visibleID := "pub-" + uuid.NewString()[:8]
	hiddenID := "hid-" + uuid.NewString()[:8]
	if err := db.Create(&models.Client{
		UUID:   visibleID,
		Token:  "tok-" + visibleID,
		Name:   "Public Node",
		IPv4:   "203.0.113.55",
		Remark: "secret-remark",
		Hidden: false,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&models.Client{
		UUID:   hiddenID,
		Token:  "tok-" + hiddenID,
		Name:   "Hidden Node",
		IPv4:   "203.0.113.99",
		Hidden: true,
	}).Error; err != nil {
		t.Fatal(err)
	}
	pingTask := models.PingTask{
		Name:      "Mixed Ping Task",
		Type:      "icmp",
		Target:    "198.51.100.1",
		Interval:  60,
		Clients:   models.StringArray{visibleID, hiddenID},
		DefaultOn: false,
	}
	if err := db.Create(&pingTask).Error; err != nil {
		t.Fatal(err)
	}

	themeShort := "testtheme_" + uuid.NewString()[:6]
	themeDir := filepath.Join("./data/theme", themeShort)
	if err := os.MkdirAll(filepath.Join(themeDir, "dist"), 0o755); err != nil {
		t.Fatal(err)
	}
	pngBytes := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0, 0, 0, 0}
	if err := os.WriteFile(filepath.Join(themeDir, "preview.png"), pngBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	manifestJSON := `{"name":"Test Theme","short":"` + themeShort + `","preview":"preview.png"}`
	if err := os.WriteFile(filepath.Join(themeDir, "komari-theme.json"), []byte(manifestJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(themeDir, "dist", "index.html"), []byte(`<html><body>theme-index</body></html>`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(themeDir)
		db.Where("id = ?", pingTask.Id).Delete(&models.PingTask{})
		db.Where("uuid IN ?", []string{visibleID, hiddenID}).Delete(&models.Client{})
		db.Where("uuid = ?", user.UUID).Delete(&models.Session{})
		db.Where("uuid = ?", user.UUID).Delete(&models.User{})
		_ = config.Set(config.PrivateSiteKey, false)
	})

	publicEngine := gin.New()
	publicEngine.Use(api.PublicViewerMiddleware())
	publicEngine.Use(api.PublicPrivateSiteMiddleware())
	const themeMarker = "<!-- public theme decorator -->"
	MountPublicRoutes(publicEngine, func(_ *http.Request, document []byte) []byte {
		return append(document, themeMarker...)
	})
	publicIndex := httptest.NewRecorder()
	publicEngine.ServeHTTP(publicIndex, httptest.NewRequest(http.MethodGet, "/", nil))
	if publicIndex.Code != http.StatusOK || !strings.Contains(publicIndex.Body.String(), themeMarker) {
		t.Fatalf("public theme document was not decorated: %d", publicIndex.Code)
	}
	rawTheme := httptest.NewRecorder()
	publicEngine.ServeHTTP(rawTheme, httptest.NewRequest(http.MethodGet, "/themes/"+themeShort+"/dist/index.html", nil))
	if rawTheme.Code != http.StatusOK || strings.Contains(rawTheme.Body.String(), themeMarker) {
		t.Fatalf("raw theme asset should bypass decoration: %d", rawTheme.Code)
	}

	panelEngine := gin.New()
	panelEngine.Use(api.IdentityMiddleware())
	panelEngine.Use(api.PrivateSiteMiddleware())
	closeExports := MountPanelRoutes(panelEngine)
	t.Cleanup(func() { _ = closeExports() })

	// 1. Public Listener returns 404 for management pages, admin APIs, and control-plane Connect RPCs.
	for _, item := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/admin"},
		{http.MethodGet, "/admin/settings/theme"},
		{http.MethodGet, "/login"},
		{http.MethodGet, "/terminal"},
		{http.MethodGet, "/bind"},
		{http.MethodPost, "/api/login"},
		{http.MethodGet, "/api/admin/settings/"},
		{http.MethodPost, "/komari.exec.v1.ExecutionService/CreateExecution"},
		{http.MethodPost, "/komari.enrollment.v1.EnrollmentAdminService/ListPendingEnrollments"},
		{http.MethodPost, "/komari.metrics.v1.MetricsService/SubmitMetrics"},
		{http.MethodPost, "/komari.browser.v1.BrowserService/GetTrafficTrend"},
	} {
		req := httptest.NewRequest(item.method, item.path, nil)
		req.AddCookie(&http.Cookie{Name: security.SessionCookieName, Value: session})
		rec := httptest.NewRecorder()
		publicEngine.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("Public listener %s %s = %d, want 404", item.method, item.path, rec.Code)
		}
	}

	// 2. Public Listener ignores administrator session cookies on RPC2 and Connect reads.
	rpcBody, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "common:getNodes",
		"params":  map[string]any{},
	})
	rpcReq := httptest.NewRequest(http.MethodPost, "/api/rpc2", bytes.NewReader(rpcBody))
	rpcReq.Header.Set("Content-Type", "application/json")
	rpcReq.AddCookie(&http.Cookie{Name: security.SessionCookieName, Value: session})
	rpcRec := httptest.NewRecorder()
	publicEngine.ServeHTTP(rpcRec, rpcReq)
	if rpcRec.Code != http.StatusOK {
		t.Fatalf("Public /api/rpc2 status = %d", rpcRec.Code)
	}
	bodyText := rpcRec.Body.String()
	if strings.Contains(bodyText, hiddenID) || strings.Contains(bodyText, "203.0.113.55") || strings.Contains(bodyText, "secret-remark") {
		t.Fatalf("Public /api/rpc2 leaked hidden node or private fields with admin cookie: %s", bodyText)
	}

	listAgentsReq := httptest.NewRequest(http.MethodPost, "/komari.browser.v1.BrowserService/ListAgents", strings.NewReader(`{}`))
	listAgentsReq.Header.Set("Content-Type", "application/json")
	listAgentsReq.AddCookie(&http.Cookie{Name: security.SessionCookieName, Value: session})
	listAgentsRec := httptest.NewRecorder()
	publicEngine.ServeHTTP(listAgentsRec, listAgentsReq)
	if listAgentsRec.Code != http.StatusOK {
		t.Fatalf("Public BrowserService/ListAgents status = %d body=%s", listAgentsRec.Code, listAgentsRec.Body.String())
	}
	if strings.Contains(listAgentsRec.Body.String(), hiddenID) || strings.Contains(listAgentsRec.Body.String(), "203.0.113.55") {
		t.Fatalf("Public BrowserService/ListAgents leaked hidden node or real IP with admin cookie: %s", listAgentsRec.Body.String())
	}

	// 2b. Public Ping task list (/api/task/ping) and common:getRecords(type=ping)
	//     must strip Hidden node UUIDs from task client lists even with an admin cookie.
	pingTasksReq := httptest.NewRequest(http.MethodGet, "/api/task/ping", nil)
	pingTasksReq.AddCookie(&http.Cookie{Name: security.SessionCookieName, Value: session})
	pingTasksRec := httptest.NewRecorder()
	publicEngine.ServeHTTP(pingTasksRec, pingTasksReq)
	if pingTasksRec.Code != http.StatusOK {
		t.Fatalf("Public /api/task/ping status = %d", pingTasksRec.Code)
	}
	if !strings.Contains(pingTasksRec.Body.String(), visibleID) || strings.Contains(pingTasksRec.Body.String(), hiddenID) {
		t.Fatalf("Public /api/task/ping projection error (must include visible and exclude hidden): %s", pingTasksRec.Body.String())
	}

	pingRecordsBody, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "common:getRecords",
		"params": map[string]any{
			"type":    "ping",
			"task_id": pingTask.Id,
			"hours":   1,
		},
	})
	pingRecordsReq := httptest.NewRequest(http.MethodPost, "/api/rpc2", bytes.NewReader(pingRecordsBody))
	pingRecordsReq.Header.Set("Content-Type", "application/json")
	pingRecordsReq.AddCookie(&http.Cookie{Name: security.SessionCookieName, Value: session})
	pingRecordsRec := httptest.NewRecorder()
	publicEngine.ServeHTTP(pingRecordsRec, pingRecordsReq)
	if pingRecordsRec.Code != http.StatusOK {
		t.Fatalf("Public common:getRecords(type=ping) status = %d", pingRecordsRec.Code)
	}
	if !strings.Contains(pingRecordsRec.Body.String(), visibleID) || strings.Contains(pingRecordsRec.Body.String(), hiddenID) {
		t.Fatalf("Public common:getRecords(type=ping) leaked hidden client UUID in task summary: %s", pingRecordsRec.Body.String())
	}

	// 3. Private site on Public Listener directs to KOMARI_PANEL_ORIGIN without honoring admin cookie,
	//    while unmounted management/control-plane routes STILL return hard 404 (never 401).
	if err := config.Set(config.PrivateSiteKey, true); err != nil {
		t.Fatal(err)
	}
	privPageReq := httptest.NewRequest(http.MethodGet, "/", nil)
	privPageReq.AddCookie(&http.Cookie{Name: security.SessionCookieName, Value: session})
	privPageRec := httptest.NewRecorder()
	publicEngine.ServeHTTP(privPageRec, privPageReq)
	if privPageRec.Code != http.StatusOK || !strings.Contains(privPageRec.Body.String(), "https://panel.example.com/login") ||
		strings.Contains(privPageRec.Body.String(), themeMarker) {
		t.Fatalf("Public private-site notice missing panel login link: code=%d body=%s", privPageRec.Code, privPageRec.Body.String())
	}
	privNodesReq := httptest.NewRequest(http.MethodGet, "/api/nodes", nil)
	privNodesReq.AddCookie(&http.Cookie{Name: security.SessionCookieName, Value: session})
	privNodesRec := httptest.NewRecorder()
	publicEngine.ServeHTTP(privNodesRec, privNodesReq)
	if privNodesRec.Code != http.StatusUnauthorized {
		t.Fatalf("Public /api/nodes under private_site with admin cookie = %d, want 401", privNodesRec.Code)
	}
	const sharePath = "/api/admin/settings/temporary-share"
	t.Cleanup(func() { _ = security.RevokeTempShare() })
	shareRequest := func(method, body string, authenticated, verified bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "https://panel.example.com"+sharePath, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if authenticated {
			req.AddCookie(&http.Cookie{Name: security.SessionCookieName, Value: session})
		}
		if verified {
			secret, _, err := accounts.Generate2Fa()
			if err != nil {
				t.Fatal(err)
			}
			if err := accounts.Enable2Fa(user.UUID, secret); err != nil {
				t.Fatal(err)
			}
			code, err := totp.GenerateCode(secret, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("X-2FA-Code", code)
		}
		rec := httptest.NewRecorder()
		panelEngine.ServeHTTP(rec, req)
		return rec
	}
	if rec := shareRequest(http.MethodPost, `{"hours":1}`, false, false); rec.Code == http.StatusOK {
		t.Fatal("anonymous request created a temporary share")
	}
	if rec := shareRequest(http.MethodPost, `{"hours":0}`, true, false); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid temporary share duration returned %d", rec.Code)
	}
	if rec := shareRequest(http.MethodPost, `{"hours":1}`, true, false); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unverified temporary-share creation returned %d", rec.Code)
	}
	create := func() security.TempShare {
		t.Helper()
		rec := shareRequest(http.MethodPost, `{"hours":1}`, true, true)
		var payload struct {
			Data security.TempShare `json:"data"`
		}
		if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" ||
			json.Unmarshal(rec.Body.Bytes(), &payload) != nil || len(payload.Data.Token) != 43 ||
			payload.Data.ExpiresAt <= time.Now().Unix() {
			t.Fatalf("temporary share response was invalid: HTTP %d", rec.Code)
		}
		return payload.Data
	}
	publicRequest := func(address string, cookie *http.Cookie) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, address, nil)
		if cookie != nil {
			req.AddCookie(cookie)
		}
		rec := httptest.NewRecorder()
		publicEngine.ServeHTTP(rec, req)
		return rec
	}
	share := create()
	shareURL := "https://status.example.com/share?temp_key=" + share.Token
	if rec := publicRequest("http"+strings.TrimPrefix(shareURL, "https"), nil); rec.Code != http.StatusForbidden {
		t.Fatalf("HTTP share redemption returned %d", rec.Code)
	}
	redeem := publicRequest(shareURL, nil)
	cookies := redeem.Result().Cookies()
	if redeem.Code != http.StatusSeeOther || redeem.Header().Get("Location") != "/" ||
		redeem.Header().Get("Cache-Control") != "no-store" || redeem.Header().Get("Referrer-Policy") != "no-referrer" ||
		len(cookies) != 1 {
		t.Fatalf("share redemption returned HTTP %d with %d cookies", redeem.Code, len(cookies))
	}
	cookie := cookies[0]
	if cookie.Name != security.TempShareCookieName || cookie.Value == share.Token ||
		!cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode ||
		cookie.Path != "/" || cookie.Domain != "" || cookie.MaxAge < 1 || cookie.MaxAge > 3600 {
		t.Fatal("share cookie did not preserve the expected browser protections")
	}
	const nodesURL = "https://status.example.com/api/nodes"
	if rec := publicRequest(nodesURL, cookie); rec.Code != http.StatusOK {
		t.Fatalf("active share could not reach public data: HTTP %d", rec.Code)
	}
	if rec := publicRequest(nodesURL, &http.Cookie{Name: "temp_key", Value: share.Token}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("legacy share cookie returned HTTP %d", rec.Code)
	}
	if rotated := create(); rotated.Token == share.Token {
		t.Fatal("rotated share reused the previous token")
	}
	if rec := publicRequest(nodesURL, cookie); rec.Code != http.StatusUnauthorized {
		t.Fatalf("rotated cookie returned HTTP %d", rec.Code)
	}
	if rec := publicRequest(shareURL, nil); len(rec.Result().Cookies()) != 0 {
		t.Fatal("old URL could still issue a cookie")
	}
	active := create()
	activeRedemption := publicRequest("https://status.example.com/share?temp_key="+active.Token, nil)
	activeCookies := activeRedemption.Result().Cookies()
	if len(activeCookies) != 1 {
		t.Fatal("new share didn't receive a cookie")
	}
	if rec := shareRequest(http.MethodDelete, "", true, false); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unverified temporary-share revocation returned %d", rec.Code)
	}
	if rec := shareRequest(http.MethodDelete, "", true, true); rec.Code != http.StatusOK {
		t.Fatalf("share revocation returned HTTP %d", rec.Code)
	}
	if rec := publicRequest(nodesURL, activeCookies[0]); rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked cookie returned HTTP %d", rec.Code)
	}
	for _, item := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/admin"},
		{http.MethodGet, "/login"},
		{http.MethodGet, "/terminal"},
		{http.MethodGet, "/bind"},
		{http.MethodPost, "/api/login"},
		{http.MethodGet, "/api/admin/settings/"},
		{http.MethodPost, "/komari.exec.v1.ExecutionService/CreateExecution"},
		{http.MethodPost, "/komari.enrollment.v1.EnrollmentAdminService/ListPendingEnrollments"},
		{http.MethodPost, "/komari.metrics.v1.MetricsService/SubmitMetrics"},
		{http.MethodPost, "/komari.browser.v1.BrowserService/GetTrafficTrend"},
	} {
		req := httptest.NewRequest(item.method, item.path, nil)
		req.AddCookie(&http.Cookie{Name: security.SessionCookieName, Value: session})
		rec := httptest.NewRecorder()
		publicEngine.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("Public listener under private_site %s %s = %d, want hard 404", item.method, item.path, rec.Code)
		}
	}
	_ = config.Set(config.PrivateSiteKey, false)

	// 4. Panel Listener returns 404 for third-party theme static paths and /api/rpc2,
	//    while serving admin theme manifest/preview safely.
	for _, item := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/themes/" + themeShort + "/komari-theme.json"},
		{http.MethodGet, "/themes/" + themeShort + "/dist/index.html"},
		{http.MethodPost, "/api/rpc2"},
		{http.MethodGet, "/api/rpc2"},
		{http.MethodGet, "/api/nodes"},
		{http.MethodGet, "/api/clients"},
	} {
		req := httptest.NewRequest(item.method, item.path, nil)
		req.AddCookie(&http.Cookie{Name: security.SessionCookieName, Value: session})
		rec := httptest.NewRecorder()
		panelEngine.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("Panel listener %s %s = %d, want 404", item.method, item.path, rec.Code)
		}
	}

	manifestReq := httptest.NewRequest(http.MethodGet, "/api/admin/theme/"+themeShort+"/manifest", nil)
	manifestReq.AddCookie(&http.Cookie{Name: security.SessionCookieName, Value: session})
	manifestRec := httptest.NewRecorder()
	panelEngine.ServeHTTP(manifestRec, manifestReq)
	if manifestRec.Code != http.StatusOK || !strings.Contains(manifestRec.Body.String(), themeShort) {
		t.Fatalf("Panel theme manifest status=%d body=%s", manifestRec.Code, manifestRec.Body.String())
	}

	previewReq := httptest.NewRequest(http.MethodGet, "/api/admin/theme/"+themeShort+"/preview", nil)
	previewReq.AddCookie(&http.Cookie{Name: security.SessionCookieName, Value: session})
	previewRec := httptest.NewRecorder()
	panelEngine.ServeHTTP(previewRec, previewReq)
	if previewRec.Code != http.StatusOK || previewRec.Header().Get("Content-Type") != "image/png" || previewRec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("Panel theme preview status=%d content-type=%q", previewRec.Code, previewRec.Header().Get("Content-Type"))
	}

	// Polyglot/HTML disguised as .png must be rejected with 415.
	if err := os.WriteFile(filepath.Join(themeDir, "preview.png"), []byte("<svg onload=alert(1)>"), 0o644); err != nil {
		t.Fatal(err)
	}
	badPreviewReq := httptest.NewRequest(http.MethodGet, "/api/admin/theme/"+themeShort+"/preview", nil)
	badPreviewReq.AddCookie(&http.Cookie{Name: security.SessionCookieName, Value: session})
	badPreviewRec := httptest.NewRecorder()
	panelEngine.ServeHTTP(badPreviewRec, badPreviewReq)
	if badPreviewRec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("Disguised SVG preview status=%d, want 415", badPreviewRec.Code)
	}
}
