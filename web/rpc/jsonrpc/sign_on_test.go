package jsonrpc

import (
	"context"
	"testing"

	"github.com/komari-monitor/komari/pkg/config"
	"github.com/komari-monitor/komari/pkg/rpc"
)

func TestLoginSettingsRejectNonInteractiveRPCBeforeWrites(t *testing.T) {
	for _, principal := range []*rpc.Principal{rpc.NewAPIKeyPrincipal(), rpc.NewAgentPrincipal("node"), rpc.NewUserPrincipal("plugin-internal")} {
		ctx := rpc.NewContextWithMeta(context.Background(), &rpc.ContextMeta{Principal: principal})
		for _, key := range loginSettingKeys {
			_, err := adminEditSettings(ctx, &rpc.JsonRpcRequest{Params: map[string]any{key: "new-value"}})
			if err == nil || err.Code != rpc.PermissionDenied {
				t.Fatalf("%s accepted without login-settings 2FA: %v", key, err)
			}
		}
		for _, call := range []rpc.Handler{adminGetOidc, adminSetOidc} {
			_, err := call(ctx, &rpc.JsonRpcRequest{Params: map[string]any{}})
			if err == nil || err.Code != rpc.PermissionDenied {
				t.Fatalf("OIDC accepted without login-settings 2FA: %v", err)
			}
		}
	}
}

func TestLoginSettingsOnlyProtectTheirOwnKeys(t *testing.T) {
	if loginSettingsTouched(map[string]any{"sitename": "custom", config.NotificationDigestEnabledKey: true}) {
		t.Fatal("unrelated settings require login-settings grant")
	}
	if !loginSettingsTouched(map[string]any{config.ApiKeyKey: ""}) {
		t.Fatal("clearing API key must also require 2FA")
	}
}
