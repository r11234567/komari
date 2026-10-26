package plugingateway

import (
	"context"
	"errors"
	"testing"

	"github.com/komari-monitor/komari/pkg/rpc"
)

func TestManagementRequiresAdminBeforeSideEffects(t *testing.T) {
	for _, principal := range []*rpc.Principal{nil, rpc.NewAnonymousPrincipal(), rpc.NewAgentPrincipal("node")} {
		ctx := rpc.NewContextWithMeta(context.Background(), &rpc.ContextMeta{Principal: principal})
		if _, err := List(ctx); !errors.Is(err, ErrPermissionDenied) {
			t.Fatalf("list: %v", err)
		}
		if err := SetEnabled(ctx, "example", true, true); !errors.Is(err, ErrPermissionDenied) {
			t.Fatalf("enable: %v", err)
		}
		if _, err := Logs(ctx, "example"); !errors.Is(err, ErrPermissionDenied) {
			t.Fatalf("logs: %v", err)
		}
		if err := Delete(ctx, "example"); !errors.Is(err, ErrPermissionDenied) {
			t.Fatalf("delete: %v", err)
		}
		if _, _, err := Configuration(ctx, "example"); !errors.Is(err, ErrPermissionDenied) {
			t.Fatalf("get config: %v", err)
		}
		if err := SaveConfiguration(ctx, "example", nil); !errors.Is(err, ErrPermissionDenied) {
			t.Fatalf("save config: %v", err)
		}
	}
}

func TestManagementValidatesNamesAndCancellation(t *testing.T) {
	ctx := rpc.NewContextWithMeta(context.Background(), &rpc.ContextMeta{Principal: rpc.NewUserPrincipal("user")})
	if err := SetEnabled(ctx, " \t", true, true); !errors.Is(err, ErrShortRequired) {
		t.Fatalf("blank name: %v", err)
	}
	if _, _, err := Configuration(ctx, ""); !errors.Is(err, ErrShortRequired) {
		t.Fatalf("blank config: %v", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := Delete(canceled, "example"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled delete: %v", err)
	}
}
