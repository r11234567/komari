package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/pkg/rpc"
	"github.com/komari-monitor/komari/utils/messageSender"
)

// TestPluginRPCRegistrationAndLifecycle verifies that:
// 1. Plugins can register custom RPC methods via server.registerRPC.
// 2. These methods are marked as plugin methods in pkg/rpc.
// 3. Invoking the method runs the JS handler and returns results / custom errors.
// 4. When the plugin is unloaded, registered RPC methods are cleanly unregistered.
func TestPluginRPCRegistrationAndLifecycle(t *testing.T) {
	withTempDataDir(t)
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	Init(engine)

	zipPath := writePluginZip(t, map[string]string{
		"komari-plugin.json": `{"name":"RPCTest","short":"rpctest","version":"1.0.0","permissions":{"allowSystemRPC":true,"timeout":5}}`,
		"script.js": `
			const server = require("server");
			function load() {
				server.registerRPC("rpctest:echo", (params) => {
					return { echo: params ? params.msg : "empty" };
				});
				server.registerRPC("rpctest:error", (params) => {
					const err = new Error("custom plugin error");
					err.code = -32001;
					err.data = { detail: "something failed" };
					throw err;
				});
			}
		`,
	})
	if _, err := InstallZip(zipPath); err != nil {
		t.Fatal(err)
	}
	if err := SetEnabled("rpctest", true, true); err != nil {
		t.Fatal(err)
	}

	// Verify method marker
	if !rpc.IsPluginMethod("rpctest:echo") {
		t.Fatal("rpctest:echo was not marked as a plugin method")
	}

	// Call rpctest:echo directly via rpc.CallWithContext
	ctx := context.Background()
	resp := rpc.CallWithContext(ctx, 1, "rpctest:echo", map[string]any{"msg": "hello from test"})
	if resp.Error != nil {
		t.Fatalf("call failed: %v", resp.Error)
	}
	resMap, ok := resp.Result.(map[string]any)
	if !ok || resMap["echo"] != "hello from test" {
		t.Fatalf("unexpected call result: %#v", resp.Result)
	}

	// Call rpctest:error -> receives custom code and message
	errResp := rpc.CallWithContext(ctx, 2, "rpctest:error", nil)
	if errResp.Error == nil {
		t.Fatal("expected error response from rpctest:error")
	}
	if errResp.Error.Code != -32001 || errResp.Error.Message != "custom plugin error" {
		t.Fatalf("unexpected rpc error: %#v", errResp.Error)
	}

	// Unload plugin -> method unregistered & marker removed
	if err := SetEnabled("rpctest", false, false); err != nil {
		t.Fatal(err)
	}
	if rpc.IsPluginMethod("rpctest:echo") {
		t.Fatal("rpctest:echo must no longer be marked after unload")
	}
	afterUnloadResp := rpc.CallWithContext(ctx, 3, "rpctest:echo", nil)
	if afterUnloadResp.Error == nil || afterUnloadResp.Error.Code != rpc.MethodNotFound {
		t.Fatalf("unloaded plugin method must return method not found, got: %#v", afterUnloadResp)
	}
}

// TestPluginInterPluginAndSystemRPCCall verifies server.call():
// 1. Calling system RPC methods (with admin privileges).
// 2. Inter-plugin RPC calling between two loaded plugins.
// 3. Permission enforcement (allowSystemRPC: false rejects server.call).
func TestPluginInterPluginAndSystemRPCCall(t *testing.T) {
	withTempDataDir(t)
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	Init(engine)

	// Register a mock system rpc method for testing
	systemCalled := false
	_ = rpc.Register("admin:testSystemAction", func(ctx context.Context, req *rpc.JsonRpcRequest) (any, *rpc.JsonRpcError) {
		meta := rpc.MetaFromContext(ctx)
		if meta == nil || meta.Principal == nil || !meta.Principal.HasRole(rpc.RoleAdmin) {
			return nil, &rpc.JsonRpcError{Code: rpc.PermissionDenied, Message: "admin required"}
		}
		systemCalled = true
		return map[string]any{"status": "ok", "system": true}, nil
	})
	t.Cleanup(func() { rpc.Unregister("admin:testSystemAction") })

	// Plugin A: provider plugin registering pluginA:add
	zipA := writePluginZip(t, map[string]string{
		"komari-plugin.json": `{"name":"PluginA","short":"plugin-a","version":"1.0.0"}`,
		"script.js": `
			const server = require("server");
			function load() {
				server.registerRPC("pluginA:add", (params) => {
					return { sum: (params.a || 0) + (params.b || 0) };
				});
			}
		`,
	})
	if _, err := InstallZip(zipA); err != nil {
		t.Fatal(err)
	}
	if err := SetEnabled("plugin-a", true, true); err != nil {
		t.Fatal(err)
	}

	// Plugin B: consumer plugin with allowSystemRPC: true calling pluginA:add and admin:testSystemAction
	zipB := writePluginZip(t, map[string]string{
		"komari-plugin.json": `{"name":"PluginB","short":"plugin-b","version":"1.0.0","permissions":{"allowSystemRPC":true,"allowRoutes":true,"timeout":5}}`,
		"script.js": `
			const server = require("server");
			function load() {
				server.route("GET", "/test-calls", async (req, res) => {
					try {
						const addRes = await server.call("pluginA:add", { a: 15, b: 25 });
						const sysRes = await server.call("admin:testSystemAction");
						res.statusCode = 200;
						res.setHeader("Content-Type", "application/json");
						res.end(JSON.stringify({ add: addRes, sys: sysRes }));
					} catch (e) {
						res.statusCode = 500;
						res.setHeader("Content-Type", "application/json");
						res.end(JSON.stringify({ error: e.message }));
					}
				});
			}
		`,
	})
	if _, err := InstallZip(zipB); err != nil {
		t.Fatal(err)
	}
	if err := SetEnabled("plugin-b", true, true); err != nil {
		t.Fatal(err)
	}

	// Invoke /test-calls
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/test-calls", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("call failed status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	addRes, _ := out["add"].(map[string]any)
	if addRes["sum"] != float64(40) {
		t.Fatalf("expected sum 40, got %#v", addRes)
	}
	if !systemCalled {
		t.Fatal("expected admin:testSystemAction to be called with admin credentials")
	}

	// Plugin C: without allowSystemRPC attempting server.call
	zipC := writePluginZip(t, map[string]string{
		"komari-plugin.json": `{"name":"PluginC","short":"plugin-c","version":"1.0.0","permissions":{"allowSystemRPC":false,"allowRoutes":true,"timeout":5}}`,
		"script.js": `
			const server = require("server");
			function load() {
				server.route("GET", "/test-unauthorized", async (req, res) => {
					try {
						await server.call("pluginA:add", { a: 1, b: 2 });
						res.statusCode = 200;
						res.end(JSON.stringify({ ok: true }));
					} catch (e) {
						res.statusCode = 403;
						res.end(JSON.stringify({ error: e.message }));
					}
				});
			}
		`,
	})
	if _, err := InstallZip(zipC); err != nil {
		t.Fatal(err)
	}
	if err := SetEnabled("plugin-c", true, true); err != nil {
		t.Fatal(err)
	}

	recC := httptest.NewRecorder()
	engine.ServeHTTP(recC, httptest.NewRequest(http.MethodGet, "/test-unauthorized", nil))
	if recC.Code != http.StatusForbidden {
		t.Fatalf("expected 403 forbidden without allowSystemRPC, got %d: %s", recC.Code, recC.Body.String())
	}
}

// TestPluginNotificationChannelRegistrationAndSend verifies:
// 1. server.registerNotificationChannel registers custom notification channel into messageSender.
// 2. Notification dispatch calls the JS handler with event payload & config.
// 3. Async Promise fulfillment and rejection work properly.
// 4. Plugin unload removes the notification channel.
func TestPluginNotificationChannelRegistrationAndSend(t *testing.T) {
	withTempDataDir(t)
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	Init(engine)

	zipPath := writePluginZip(t, map[string]string{
		"komari-plugin.json": `{"name":"Notifier","short":"notifier","version":"1.0.0","permissions":{"timeout":5}}`,
		"script.js": `
			const server = require("server");
			function load() {
				server.registerNotificationChannel("custom-notify", {
					type: "managed",
					data: [
						{ key: "webhook_url", name: "Webhook URL", type: "string", default: "http://example.com" }
					]
				}, async (notification, config) => {
					if (notification.title === "fail-me") {
						throw new Error("delivery rejected by channel");
					}
					return true;
				});
			}
		`,
	})
	if _, err := InstallZip(zipPath); err != nil {
		t.Fatal(err)
	}
	if err := SetEnabled("notifier", true, true); err != nil {
		t.Fatal(err)
	}

	// 1. Channel should be registered in messageSender
	if !messageSender.NotificationChannelRegistered("custom-notify") {
		t.Fatal("custom-notify was not registered in messageSender")
	}

	channels := messageSender.ListNotificationChannels()
	found := false
	for _, ch := range channels {
		if ch.ID == "custom-notify" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("custom-notify not found in ListNotificationChannels")
	}

	// 2. Send successful notification
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := messageSender.CallNotificationChannel(ctx, "custom-notify", messageSender.Notification{
		Title:   "test-event",
		Message: "Notification body test",
		Event: models.EventMessage{
			Event: "test",
		},
	})
	if err != nil {
		t.Fatalf("CallNotificationChannel failed: %v", err)
	}

	// 3. Send notification that triggers promise rejection
	errFail := messageSender.CallNotificationChannel(ctx, "custom-notify", messageSender.Notification{
		Title:   "fail-me",
		Message: "Should fail",
	})
	if errFail == nil {
		t.Fatal("expected CallNotificationChannel to fail with rejection")
	}
	if !bytes.Contains([]byte(errFail.Error()), []byte("delivery rejected by channel")) {
		t.Fatalf("unexpected error message: %v", errFail)
	}

	// 4. Reload plugin -> still registered
	if err := Reload("notifier"); err != nil {
		t.Fatalf("Reload failed: %v", err)
	}
	if !messageSender.NotificationChannelRegistered("custom-notify") {
		t.Fatal("custom-notify should still be registered after reload")
	}

	// 5. Unload plugin -> channel unregistered
	if err := SetEnabled("notifier", false, false); err != nil {
		t.Fatal(err)
	}
	if messageSender.NotificationChannelRegistered("custom-notify") {
		t.Fatal("custom-notify must be unregistered after unload")
	}
}
