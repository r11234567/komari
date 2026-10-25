package connectapi

import (
	"context"
	"testing"

	"connectrpc.com/connect"
	"github.com/komari-monitor/komari/pkg/rpc"
	filev1 "github.com/r11234567/komari-proto/gen/go/komari/file/v1"
	workspacev1 "github.com/r11234567/komari-proto/gen/go/komari/workspace/v1"
)

func TestWorkspaceRejectsNonInteractivePrincipals(t *testing.T) {
	for _, principal := range []*rpc.Principal{rpc.NewAnonymousPrincipal(), rpc.NewAPIKeyPrincipal(), rpc.NewAgentPrincipal("agent"), rpc.PrincipalFromRole(rpc.RoleAdmin)} {
		ctx := rpc.NewContextWithMeta(context.Background(), &rpc.ContextMeta{Principal: principal})
		_, err := (&workspaceService{}).UnlockWorkspace(ctx, connect.NewRequest(&workspacev1.UnlockWorkspaceRequest{AgentId: "agent", SystemUser: "root"}))
		if connect.CodeOf(err) != connect.CodeUnauthenticated {
			t.Fatalf("principal %v accepted: %v", principal, err)
		}
		_, err = (&fileService{}).ListDirectory(ctx, connect.NewRequest(&filev1.ListDirectoryRequest{AgentId: "agent", Path: "/"}))
		if connect.CodeOf(err) != connect.CodeUnauthenticated {
			t.Fatalf("unlocked read for %v: %v", principal, err)
		}
	}
}

func TestWorkspaceProceduresHaveExplicitAdminPolicy(t *testing.T) {
	policies := newPolicyInterceptor().policies
	for _, method := range []string{"UnlockWorkspace", "GetWorkspace", "TouchWorkspace", "LockWorkspace"} {
		policy, ok := policies["/komari.workspace.v1.WorkspaceService/"+method]
		if !ok || policy.role != rpc.RoleAdmin {
			t.Fatalf("missing admin boundary for %s", method)
		}
	}
}
