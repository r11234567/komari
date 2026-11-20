package plugin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/pkg/rpc"
)

func TestPluginSystemRPCBoundaryMatrix(t *testing.T) {
	withTempDataDir(t)
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	Init(engine)

	var publicCalled atomic.Int32
	var adminCalled atomic.Int32
	var seenRole string
	cancelCh := make(chan struct{}, 1)

	_ = rpc.Register("public:testReadVersion", func(ctx context.Context, _ *rpc.JsonRpcRequest) (any, *rpc.JsonRpcError) {
		publicCalled.Add(1)
		if meta := rpc.MetaFromContext(ctx); meta != nil && meta.Principal != nil {
			seenRole = meta.Principal.PrimaryRole()
		}
		return map[string]any{"version": "p2b-test"}, nil
	})
	rpc.Allow("public:testReadVersion", rpc.RoleGuest)
	_ = rpc.Register("admin:testPrivilegedOp", func(_ context.Context, _ *rpc.JsonRpcRequest) (any, *rpc.JsonRpcError) {
		adminCalled.Add(1)
		return map[string]any{"ok": true}, nil
	})
	_ = rpc.Register("public:testBlockingRead", func(ctx context.Context, _ *rpc.JsonRpcRequest) (any, *rpc.JsonRpcError) {
		select {
		case <-ctx.Done():
			select {
			case cancelCh <- struct{}{}:
			default:
			}
			return nil, rpc.MakeError(rpc.InternalError, "canceled", nil)
		case <-time.After(5 * time.Second):
			return map[string]any{"ok": true}, nil
		}
	})
	rpc.Allow("public:testBlockingRead", rpc.RoleGuest)
	t.Cleanup(func() {
		SetSystemRPCAllowlist()
		rpc.Unregister("public:testReadVersion")
		rpc.Unregister("admin:testPrivilegedOp")
		rpc.Unregister("public:testBlockingRead")
	})

	zipPath := writePluginZip(t, map[string]string{
		"komari-plugin.json": `{"name":"Boundary","short":"boundary","version":"1.0.0","permissions":{"allowSystemRPC":true,"allowRoutes":true,"timeout":1}}`,
		"script.js": `
			const server = require("server");
			function load() {
				server.route("GET", "/probe", async (req, res) => {
					const method = req.query.m || "";
					try {
						const out = await server.call(method, {});
						res.statusCode = 200;
						res.setHeader("Content-Type", "application/json");
						res.end(JSON.stringify({ ok: true, result: out }));
					} catch (e) {
						res.statusCode = 403;
						res.setHeader("Content-Type", "application/json");
						res.end(JSON.stringify({ ok: false, code: e.code, message: e.message }));
					}
				});
			}
		`,
	})
	if _, err := InstallZip(zipPath); err != nil {
		t.Fatal(err)
	}
	if err := SetEnabled("boundary", true, true); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = SetEnabled("boundary", false, false) })

	call := func(method string) (int, map[string]any) {
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/probe?m="+method, nil))
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return rec.Code, body
	}

	// 1. Default empty allowlist denies even registered public read-only method before dispatch.
	SetSystemRPCAllowlist()
	status, body := call("public:testReadVersion")
	if status != http.StatusForbidden || body["code"] != float64(rpc.PermissionDenied) || publicCalled.Load() != 0 {
		t.Fatalf("empty allowlist failed: status=%d body=%+v called=%d", status, body, publicCalled.Load())
	}

	// 2. Removed SQL methods & unknown methods return MethodNotFound (-32601).
	for _, removed := range []string{"admin:dbQuery", "admin:dbExec", "admin:dbTables", "unknown:method"} {
		status, body = call(removed)
		if status != http.StatusForbidden || body["code"] != float64(rpc.MethodNotFound) {
			t.Fatalf("removed/unknown method %q returned status=%d body=%+v, want code %d", removed, status, body, rpc.MethodNotFound)
		}
	}

	// 3. Registered privileged admin:* method is rejected with PermissionDenied even if passed to allowlist.
	SetSystemRPCAllowlist("public:testReadVersion", "public:testBlockingRead", "admin:testPrivilegedOp")
	status, body = call("admin:testPrivilegedOp")
	if status != http.StatusForbidden || body["code"] != float64(rpc.PermissionDenied) || adminCalled.Load() != 0 {
		t.Fatalf("admin method was not blocked pre-dispatch: status=%d body=%+v called=%d", status, body, adminCalled.Load())
	}

	// 4. Explicit public read-only allowlist positive case runs with RoleGuest (never RoleAdmin).
	status, body = call("public:testReadVersion")
	if status != http.StatusOK || body["ok"] != true || publicCalled.Load() != 1 || seenRole != rpc.RoleGuest {
		t.Fatalf("public read-only positive case failed: status=%d body=%+v role=%q", status, body, seenRole)
	}

	// 5. Context cancellation propagates from plugin timeout into RPC handler.
	status, _ = call("public:testBlockingRead")
	if status != http.StatusForbidden && status != http.StatusGatewayTimeout {
		t.Fatalf("unexpected status on blocking RPC call: %d", status)
	}
	select {
	case <-cancelCh:
	case <-time.After(2 * time.Second):
		t.Fatal("context cancellation did not propagate to RPC handler")
	}

	// 6. Revoking allowlist denies subsequent calls immediately.
	SetSystemRPCAllowlist()
	status, body = call("public:testReadVersion")
	if status != http.StatusForbidden || body["code"] != float64(rpc.PermissionDenied) {
		t.Fatalf("revoked allowlist did not deny call: status=%d body=%+v", status, body)
	}
}
