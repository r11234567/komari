package jsonrpc

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/internal/plugin"
	"github.com/komari-monitor/komari/pkg/config"
	"github.com/komari-monitor/komari/pkg/rpc"
	"github.com/komari-monitor/komari/web/api"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// TestPluginRPCGatewayAccess verifies /api/rpc2 blocks plugin, admin, and
// write methods while serving third-party read/heartbeat compatibility methods.
func TestPluginRPCGatewayAccess(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	config.SetDb(db)

	engine := gin.New()
	plugin.Init(engine)

	engine.Use(func(c *gin.Context) {
		role := c.GetHeader("X-Role")
		if role == "admin" {
			api.SetPrincipal(c, rpc.NewUserPrincipal("admin-user"))
		} else {
			api.SetPrincipal(c, rpc.NewAnonymousPrincipal())
		}
		c.Next()
	})
	engine.POST("/api/rpc2", OnRpcRequest)

	const pluginMethod = "myplugin:compute"
	if err := rpc.Register(pluginMethod, func(ctx context.Context, req *rpc.JsonRpcRequest) (any, *rpc.JsonRpcError) {
		return map[string]any{"computed": true}, nil
	}); err != nil {
		t.Fatal(err)
	}
	rpc.MarkPluginMethod(pluginMethod)
	defer rpc.Unregister(pluginMethod)

	callRPC := func(role string, method string, params any) *rpc.JsonRpcResponse {
		reqBody, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"method":  method,
			"params":  params,
		})
		req := httptest.NewRequest(http.MethodPost, "/api/rpc2", bytes.NewReader(reqBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Role", role)
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, req)

		var resp rpc.JsonRpcResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode rpc response (%s): %v, body = %s", method, err, rec.Body.String())
		}
		return &resp
	}

	// 1. Plugin methods are blocked on /api/rpc2 even for admin principals.
	for _, role := range []string{"admin", "guest"} {
		resp := callRPC(role, pluginMethod, nil)
		if resp.Error == nil || resp.Error.Code != rpc.PermissionDenied {
			t.Fatalf("plugin method must be denied on /api/rpc2 for %s, got: %#v", role, resp)
		}
	}

	// 2. Admin and visitor-audit write methods are blocked by gateway allowlist.
	for _, blocked := range []string{"admin:getClientList", "public:recordVisitorEvent"} {
		resp := callRPC("admin", blocked, nil)
		if resp.Error == nil || resp.Error.Code != rpc.PermissionDenied {
			t.Fatalf("%s must be blocked by gateway allowlist, got: %#v", blocked, resp)
		}
	}

	// 3. Read-only heartbeat rpc.ping succeeds without PermissionDenied.
	pingResp := callRPC("guest", "rpc.ping", nil)
	if pingResp.Error != nil || pingResp.Result != "pong" {
		t.Fatalf("rpc.ping failed on /api/rpc2: %#v", pingResp)
	}
}

func TestPluginConfigurationPreservesNotFound(t *testing.T) {
	ctx := rpc.NewContextWithMeta(context.Background(), &rpc.ContextMeta{Principal: rpc.NewUserPrincipal("user")})
	_, err := adminGetPluginConfiguration(ctx, &rpc.JsonRpcRequest{Params: map[string]any{"short": "../invalid-name"}})
	if err == nil || err.Code != rpc.NotFound {
		t.Fatalf("manifest lookup mapping changed: %v", err)
	}
}
