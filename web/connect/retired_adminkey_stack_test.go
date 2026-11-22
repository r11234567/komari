package connectapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"
	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/pkg/rpc"
	"github.com/komari-monitor/komari/web/api"
	"github.com/komari-monitor/komari/web/rpc/jsonrpc"
	"github.com/komari-monitor/komari/web/security"
	browserv1 "github.com/r11234567/komari-proto/gen/go/komari/browser/v1"
	browserv1connect "github.com/r11234567/komari-proto/gen/go/komari/browser/v1/browserv1connect"
	"github.com/stretchr/testify/require"
)

func TestRetiredAdminKeyFullTransportStackAndDefenseInDepth(t *testing.T) {
	_, _, session := actionFixture(t)

	// 1. Stale PrincipalAPIKey without APIToken fails closed across all authorization helpers.
	staleAPIKey := &rpc.Principal{Type: rpc.PrincipalAPIKey, IsAPIKey: true}
	require.False(t, staleAPIKey.AllowsAPIProcedure("/komari.browser.v1.BrowserService/ListAgents"))
	require.False(t, staleAPIKey.AllowsAgent("any-agent"))
	require.False(t, rpc.CheckPrincipal(staleAPIKey, "admin:getSettings"))
	require.Error(t, requireRescueAdministrator(&rpc.ContextMeta{Principal: staleAPIKey}))

	// 2. Real REST + Real Connect + Real RPC2 stack: retired Bearer (including tab-separated)
	//    never falls back to valid browser Cookie and never creates an admin principal.
	router := gin.New()
	router.Use(api.APIAuthenticationMiddleware(), api.IdentityMiddleware())
	router.GET("/api/admin/whoami", api.RequireRole(api.RoleAdmin), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"role": api.GetRole(c)})
	})
	router.POST("/api/rpc2", jsonrpc.OnRpcRequest)
	Register(router)
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)

	for _, authHeader := range []string{"Bearer retired-legacy-admin-key-123456", "Bearer\tretired-legacy-admin-key-123456"} {
		// REST admin endpoint with retired Bearer + valid session cookie must return 401.
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/admin/whoami", nil)
		req.Header.Set("Authorization", authHeader)
		req.AddCookie(&http.Cookie{Name: security.SessionCookieName, Value: session})
		resp, err := srv.Client().Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		require.Equal(t, http.StatusUnauthorized, resp.StatusCode, "REST fell back to cookie for %q", authHeader)

		// Real RPC2 endpoint with retired Bearer + valid session cookie must return 401.
		rpcBody, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "public:getMe", "params": map[string]any{}})
		rpcReq, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/rpc2", bytes.NewReader(rpcBody))
		rpcReq.Header.Set("Content-Type", "application/json")
		rpcReq.Header.Set("Authorization", authHeader)
		rpcReq.AddCookie(&http.Cookie{Name: security.SessionCookieName, Value: session})
		rpcResp, err := srv.Client().Do(rpcReq)
		require.NoError(t, err)
		_ = rpcResp.Body.Close()
		require.Equal(t, http.StatusUnauthorized, rpcResp.StatusCode, "RPC2 fell back to cookie for %q", authHeader)

		// Real Connect endpoint with retired Bearer + valid session cookie must reject.
		client := browserv1connect.NewBrowserServiceClient(srv.Client(), srv.URL)
		connectReq := connect.NewRequest(&browserv1.ListAgentsRequest{})
		connectReq.Header().Set("Authorization", authHeader)
		connectReq.Header().Set("Cookie", security.SessionCookieName+"="+session)
		_, err = client.ListAgents(t.Context(), connectReq)
		require.Error(t, err, "Connect fell back to cookie for %q", authHeader)
	}

	// 3. Direct IdentifyPrincipal without APIAuthenticationMiddleware still refuses Cookie fallback
	//    on tab-separated or space-separated Bearer headers (including plugin foreign Bearers).
	direct := gin.New()
	direct.GET("/identify", func(c *gin.Context) {
		p := api.IdentifyPrincipal(c)
		c.JSON(http.StatusOK, gin.H{"is_admin": p.HasRole(rpc.RoleAdmin), "type": int(p.Type)})
	})
	for _, header := range []string{"Bearer foreign-plugin-token", "Bearer\tforeign-plugin-token", "Bearer  retired-legacy-key"} {
		req := httptest.NewRequest(http.MethodGet, "/identify", nil)
		req.Header.Set("Authorization", header)
		req.AddCookie(&http.Cookie{Name: security.SessionCookieName, Value: session})
		rec := httptest.NewRecorder()
		direct.ServeHTTP(rec, req)
		var out struct {
			IsAdmin bool              `json:"is_admin"`
			Type    rpc.PrincipalType `json:"type"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
		require.False(t, out.IsAdmin, "IdentifyPrincipal elevated to admin for %q", header)
		require.Equal(t, rpc.PrincipalAnonymous, out.Type, "IdentifyPrincipal did not return anonymous for %q", header)
	}

	// 4. Foreign Origin with a Bearer token must be rejected by CORS without fallback.
	corsRouter := gin.New()
	corsCtrl := security.NewCorsController(true, "")
	corsRouter.Use(corsCtrl.Middleware())
	corsRouter.GET("/api/test", func(c *gin.Context) { c.Status(http.StatusOK) })
	corsReq := httptest.NewRequest(http.MethodGet, "https://panel.example.test/api/test", nil)
	corsReq.Header.Set("Origin", "https://external-client.example.net")
	corsReq.Header.Set("Authorization", "Bearer retired-legacy-key")
	corsRec := httptest.NewRecorder()
	corsRouter.ServeHTTP(corsRec, corsReq)
	require.Equal(t, http.StatusForbidden, corsRec.Code)
	require.Empty(t, corsRec.Header().Get("Access-Control-Allow-Origin"))
	require.Empty(t, corsRec.Header().Get("Access-Control-Allow-Credentials"))
}
