package connectapi

import (
	"context"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/komari-monitor/komari/database/clients"
	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/pkg/rpc"
	"github.com/komari-monitor/komari/web/filemanager"
	"github.com/komari-monitor/komari/web/remotemanagement"
	"github.com/komari-monitor/komari/web/workspace"
	"github.com/pquerna/otp/totp"
	commonv1 "github.com/r11234567/komari-proto/gen/go/komari/common/v1"
	filev1 "github.com/r11234567/komari-proto/gen/go/komari/file/v1"
	websshv1 "github.com/r11234567/komari-proto/gen/go/komari/webssh/v1"
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

// This is an in-process component check with an isolated test database and a
// synthetic Agent dispatcher, not a browser/Agent E2E or a real host operation.
func TestWorkspaceFresh2FAToAssignmentsAndCapabilityRevocation(t *testing.T) {
	db := dbcore.GetDBInstance()
	user, login := uuid.NewString(), "synthetic-login-"+uuid.NewString()
	const secret = "JBSWY3DPEHPK3PXP"
	if err := db.Create(&models.User{UUID: user, Username: "workspace-" + user, Passwd: "unused-synthetic-test", TwoFactor: secret}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&models.Session{UUID: user, Session: login, Expires: time.Now().Add(time.Hour)}).Error; err != nil {
		t.Fatal(err)
	}
	agent, _, err := clients.CreateClient()
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&models.ClientPrivilegedRevision{Client: agent, Revision: 1, Config: `{"remote_control_enabled":true,"webssh_enabled":true}`}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&models.ClientPrivilegedState{Client: agent, ProtocolVersion: 2, AppliedRevision: 1, DesiredRevision: 1}).Error; err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ctx = rpc.NewContextWithMeta(ctx, &rpc.ContextMeta{Principal: rpc.NewUserPrincipal(user), UserUUID: user, SessionToken: login})
	svc := &workspaceService{}
	unlock := &workspacev1.UnlockWorkspaceRequest{AgentId: agent, SystemUser: "alice"}
	if _, err := svc.UnlockWorkspace(ctx, connect.NewRequest(unlock)); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("missing fresh 2FA accepted: %v", err)
	}
	code, err := totp.GenerateCode(secret, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	unlock.TwoFactor = &commonv1.TwoFactorProof{Code: code}
	response, err := svc.UnlockWorkspace(ctx, connect.NewRequest(unlock))
	if err != nil {
		t.Fatal(err)
	}
	token := response.Msg.Authorization.GrantToken
	t.Cleanup(func() { _ = workspace.Default.Lock(token, user, login) })
	if _, err := svc.UnlockWorkspace(ctx, connect.NewRequest(unlock)); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("replayed fresh 2FA accepted: %v", err)
	}
	ctx = workspace.WithToken(ctx, token)
	snapshot, err := workspace.Default.Check(token, user, login, agent, false)
	if err != nil {
		t.Fatal(err)
	}
	terminal, err := remotemanagement.Create(agent, user, "", "", nil, snapshot.Grant)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { terminal.Close(websshv1.CloseReason_CLOSE_REASON_CANCELLED) })
	assignment, err := remotemanagement.Lease(ctx, agent)
	if err != nil || assignment.GetSystemUser() != "alice" {
		t.Fatalf("terminal account assignment failed: %v", err)
	}
	filemanager.SetDispatcher(func(id string, assignment *filev1.FileAssignment) bool {
		if id != agent || assignment.SystemUser != "alice" || assignment.WorkspaceId != snapshot.Grant.ID {
			t.Error("file assignment lost its account or workspace binding")
		}
		return filemanager.Resolve(id, &filev1.AgentFileEvent{RequestId: assignment.RequestId, Success: true, Path: "/home/alice"})
	})
	t.Cleanup(func() { filemanager.SetDispatcher(nil) })
	files := &fileService{}
	list := connect.NewRequest(&filev1.ListDirectoryRequest{AgentId: agent, Path: "/home/alice"})
	if _, err := files.ListDirectory(ctx, list); err != nil {
		t.Fatalf("unlocked file operation still required per-operation OTP: %v", err)
	}
	if _, err := files.ListDirectory(workspace.WithToken(ctx, ""), list); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("missing grant accepted: %v", err)
	}
	started := make(chan struct{})
	filemanager.SetDispatcher(func(string, *filev1.FileAssignment) bool { close(started); return true })
	result := make(chan error, 1)
	go func() { _, err := files.ListDirectory(ctx, list); result <- err }()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("pending file operation never dispatched")
	}
	// A pending privileged revocation must take effect before its Agent ACK.
	if err := db.Create(&models.ClientPrivilegedRevision{Client: agent, Revision: 2, Config: `{"remote_control_enabled":false,"webssh_enabled":false}`}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&models.ClientPrivilegedState{}).Where("client = ?", agent).Update("desired_revision", 2).Error; err != nil {
		t.Fatal(err)
	}
	workspace.Default.Sweep()
	select {
	case err := <-result:
		if connect.CodeOf(err) != connect.CodeCanceled {
			t.Fatalf("revoked capability did not cancel file operation: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("capability revocation left file operation running")
	}
	if _, err := files.ListDirectory(ctx, list); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("revoked grant accepted a new file operation: %v", err)
	}
	if _, err := terminal.NextEvent(ctx, 0); !errors.Is(err, remotemanagement.ErrClosed) {
		t.Fatalf("capability revocation left terminal open: %v", err)
	}
}
