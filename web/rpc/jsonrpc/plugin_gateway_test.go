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

// TestPluginRPCGatewayAccess verifies /api/rpc2 behaves as intended for plugin methods:
// - Plugin-registered methods are allowed through /api/rpc2 for admin principals.
// - Guest/unauthenticated requests to plugin methods are rejected by ACL (PermissionDenied).
// - Admin internal methods (like admin:*) remain blocked on /api/rpc2.
// - Unregistered/unloaded plugin methods are blocked by /api/rpc2.
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
	// Register RPC handler as a plugin method
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

	// 1. Admin caller calls plugin method -> Success
	adminResp := callRPC("admin", pluginMethod, nil)
	if adminResp.Error != nil {
		t.Fatalf("admin call to plugin method failed: %v", adminResp.Error)
	}
	resMap, ok := adminResp.Result.(map[string]any)
	if !ok || resMap["computed"] != true {
		t.Fatalf("unexpected result: %#v", adminResp.Result)
	}

	// 2. Guest caller calls plugin method -> Permission Denied
	guestResp := callRPC("guest", pluginMethod, nil)
	if guestResp.Error == nil || guestResp.Error.Code != rpc.PermissionDenied {
		t.Fatalf("guest call must be denied, got: %#v", guestResp)
	}

	// 3. Admin calls admin:* -> Blocked by gateway allowlist
	blockedResp := callRPC("admin", "admin:getClientList", nil)
	if blockedResp.Error == nil || blockedResp.Error.Code != rpc.PermissionDenied {
		t.Fatalf("admin:* must be blocked by gateway allowlist, got: %#v", blockedResp)
	}

	// 4. Unregister method -> Blocked by gateway allowlist
	rpc.Unregister(pluginMethod)
	unregisteredResp := callRPC("admin", pluginMethod, nil)
	if unregisteredResp.Error == nil || unregisteredResp.Error.Code != rpc.PermissionDenied {
		t.Fatalf("unregistered method must be blocked by gateway allowlist, got: %#v", unregisteredResp)
	}
}

func TestPluginConfigurationPreservesNotFound(t *testing.T) {
	ctx := rpc.NewContextWithMeta(context.Background(), &rpc.ContextMeta{Principal: rpc.NewUserPrincipal("user")})
	_, err := adminGetPluginConfiguration(ctx, &rpc.JsonRpcRequest{Params: map[string]any{"short": "../invalid-name"}})
	if err == nil || err.Code != rpc.NotFound {
		t.Fatalf("manifest lookup mapping changed: %v", err)
	}
}
