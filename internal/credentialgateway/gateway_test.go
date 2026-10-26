package credentialgateway

import (
	"context"
	"errors"
	"testing"

	credential "github.com/komari-monitor/komari/internal/trustedcredential"
	"github.com/komari-monitor/komari/pkg/rpc"
)

func TestOnlyInteractiveIdentitiesReachStorage(t *testing.T) {
	for _, p := range []*rpc.Principal{nil, rpc.NewAnonymousPrincipal(), rpc.NewAgentPrincipal("synthetic"), rpc.NewAPIKeyPrincipal(), rpc.PrincipalFromRole(rpc.RoleAdmin), rpc.NewUserPrincipal("synthetic-user")} {
		ctx := rpc.NewContextWithMeta(context.Background(), &rpc.ContextMeta{Principal: p})
		g := New()
		if _, err := g.List(ctx, ""); !errors.Is(err, credential.ErrUnauthenticated) {
			t.Fatalf("noninteractive identity: %v", err)
		}
		if _, err := g.Begin(ctx, credential.Request{}); !errors.Is(err, credential.ErrUnauthenticated) {
			t.Fatal(err)
		}
		if _, err := g.Complete(ctx, "", nil); !errors.Is(err, credential.ErrUnauthenticated) {
			t.Fatal(err)
		}
		if _, err := g.Cancel(ctx, ""); !errors.Is(err, credential.ErrUnauthenticated) {
			t.Fatal(err)
		}
		if _, err := g.Consume(ctx, "", credential.BindContact, "target"); !errors.Is(err, credential.ErrUnauthenticated) {
			t.Fatal(err)
		}
	}
}
