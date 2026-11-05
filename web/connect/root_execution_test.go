package connectapi

import (
	"fmt"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/komari-monitor/komari/database/clients"
	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/internal/credentialaction"
	credential "github.com/komari-monitor/komari/internal/trustedcredential"
	agentRuntime "github.com/komari-monitor/komari/web/agent"
	"github.com/komari-monitor/komari/web/executionapp"
	"github.com/komari-monitor/komari/web/workspace"
	commonv1 "github.com/r11234567/komari-proto/gen/go/komari/common/v1"
	execv1 "github.com/r11234567/komari-proto/gen/go/komari/exec/v1"
	workspacev1 "github.com/r11234567/komari-proto/gen/go/komari/workspace/v1"
	"github.com/stretchr/testify/require"
)

func rootPolicyNode(t *testing.T, root bool) string {
	t.Helper()
	id, _, err := clients.CreateClient()
	require.NoError(t, err)
	db := dbcore.GetDBInstance()
	config := fmt.Sprintf(`{"remote_control_enabled":true,"webssh_enabled":true,"execution_enabled":true,"root_execution_allowed":%t}`, root)
	require.NoError(t, db.Create(&models.ClientPrivilegedRevision{Client: id, Revision: 1, Config: config}).Error)
	require.NoError(t, db.Create(&models.ClientPrivilegedState{Client: id, ProtocolVersion: 2, AppliedRevision: 1, DesiredRevision: 1}).Error)
	agentRuntime.KeepAlivePresence(id, 1, time.Minute)
	agentRuntime.MarkConnectExecutionLease(id)
	t.Cleanup(func() { agentRuntime.DeleteConnectedClients(id) })
	return id
}

func TestExecutionRootSelectionRequiresPermissionAndBoundHumanProof(t *testing.T) {
	for _, tc := range []struct {
		name                                  string
		grant, requested, rootProof, succeeds bool
	}{
		{"ordinary-default", false, false, false, true},
		{"root-denied-by-default", false, true, true, false},
		{"ordinary-proof-cannot-escalate", true, true, false, false},
		{"approved-and-explicit-root", true, true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, owner, _ := actionFixture(t)
			id := rootPolicyNode(t, tc.grant)
			targets := []string{id}
			if tc.rootProof {
				targets = append(targets, "run-as:root")
			}
			token := actionAuthorization(t, ctx, credential.SensitiveOperation, credentialaction.ScopedResource("execution:create", targets...))
			response, err := (&executionService{}).CreateExecution(ctx, connect.NewRequest(&execv1.CreateExecutionRequest{AgentIds: []string{id}, Command: "id", RunAsRoot: tc.requested, TwoFactor: &commonv1.TwoFactorProof{AuthorizationToken: token}}))
			if !tc.succeeds {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			task, err := executionapp.GetOwned(response.Msg.Execution.ExecutionId, owner)
			require.NoError(t, err)
			require.Equal(t, tc.requested, task.Spec.RunAsRoot)
			_, _ = executionapp.Cancel(response.Msg.Execution.ExecutionId, "test cleanup")
		})
	}
}

func TestRootWorkspaceRejectsDefaultAndRevokesOnWithdrawal(t *testing.T) {
	ctx, owner, login := actionFixture(t)
	id := rootPolicyNode(t, false)
	token := actionAuthorization(t, ctx, credential.UnlockWorkspace, credentialaction.WorkspaceResource(id, "root"))
	request := connect.NewRequest(&workspacev1.UnlockWorkspaceRequest{AgentId: id, SystemUser: "root", TwoFactor: &commonv1.TwoFactorProof{AuthorizationToken: token}})
	_, err := (&workspaceService{}).UnlockWorkspace(ctx, request)
	require.Equal(t, connect.CodePermissionDenied, connect.CodeOf(err))
	db := dbcore.GetDBInstance()
	require.NoError(t, db.Model(&models.ClientPrivilegedRevision{}).Where("client = ? AND revision = 1", id).Update("config", `{"remote_control_enabled":true,"webssh_enabled":true,"root_execution_allowed":true}`).Error)
	response, err := (&workspaceService{}).UnlockWorkspace(ctx, request)
	require.NoError(t, err)
	grant := response.Msg.Authorization
	t.Cleanup(func() { _ = workspace.Default.Lock(grant.GrantToken, owner, login) })
	require.NoError(t, db.Create(&models.ClientPrivilegedRevision{Client: id, Revision: 2, Config: `{"remote_control_enabled":true,"webssh_enabled":true}`}).Error)
	require.NoError(t, db.Model(&models.ClientPrivilegedState{}).Where("client = ?", id).Update("desired_revision", 2).Error)
	_, err = workspace.Default.Check(grant.GrantToken, owner, login, id, false)
	require.Error(t, err)
}
