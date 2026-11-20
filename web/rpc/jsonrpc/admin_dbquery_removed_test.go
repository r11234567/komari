package jsonrpc

import (
	"context"
	"testing"

	"github.com/komari-monitor/komari/pkg/rpc"
)

func TestOnlineDatabaseSQLRPCMethodsAreRemoved(t *testing.T) {
	adminCtx := rpc.NewContextWithMeta(context.Background(), &rpc.ContextMeta{
		Principal:  rpc.NewUserPrincipal("admin-1"),
		Permission: rpc.RoleAdmin,
	})
	for _, method := range []string{"admin:dbQuery", "admin:dbExec", "admin:dbTables"} {
		if rpc.HasMethod(method) {
			t.Fatalf("retired SQL RPC method %q is still registered", method)
		}
		resp := rpc.CallWithContext(adminCtx, 1, method, map[string]any{"sql": "SELECT 1"})
		if resp == nil || resp.Error == nil || resp.Error.Code != rpc.MethodNotFound {
			t.Fatalf("method %q returned %+v, want MethodNotFound (%d)", method, resp, rpc.MethodNotFound)
		}
	}
}
