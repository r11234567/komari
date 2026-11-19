package connectapi

import (
	"fmt"
	"strings"
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
	"google.golang.org/protobuf/types/known/durationpb"
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
			targets := credentialaction.ExecutionTargets([]string{id}, "id", nil, nil, "", 0, 0, tc.rootProof)
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

func TestExecutionRejectsTamperedSpecAndIdempotencyConflict(t *testing.T) {
	ctx, owner, _ := actionFixture(t)
	id := rootPolicyNode(t, false)
	targets := credentialaction.ExecutionTargets([]string{id}, "echo safe", nil, nil, "", 0, 0, false)
	token := actionAuthorization(t, ctx, credential.SensitiveOperation, credentialaction.ScopedResource("execution:create", targets...))

	// Proof bound to "echo safe" must reject a different command without consuming the grant.
	_, err := (&executionService{}).CreateExecution(ctx, connect.NewRequest(&execv1.CreateExecutionRequest{
		AgentIds:  []string{id},
		Command:   "cat /etc/shadow",
		TwoFactor: &commonv1.TwoFactorProof{AuthorizationToken: token},
	}))
	require.Equal(t, connect.CodeNotFound, connect.CodeOf(err))

	// Same unconsumed grant succeeds for the exact bound specification with an idempotency key.
	resp1, err := (&executionService{}).CreateExecution(ctx, connect.NewRequest(&execv1.CreateExecutionRequest{
		AgentIds:       []string{id},
		Command:        "echo safe",
		IdempotencyKey: "idem-spec-check-1",
		TwoFactor:      &commonv1.TwoFactorProof{AuthorizationToken: token},
	}))
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = executionapp.Cancel(resp1.Msg.Execution.ExecutionId, "test cleanup") })

	// Same idempotency key with the exact same spec returns the existing execution.
	replayExecs, err := executionapp.CreateBatch([]string{id}, owner, "idem-spec-check-1", &execv1.ExecutionSpec{Command: "echo safe"})
	require.NoError(t, err)
	require.Len(t, replayExecs, 1)
	require.Equal(t, resp1.Msg.Execution.ExecutionId, replayExecs[0].ExecutionId)

	// Same idempotency key with a different command must conflict and map to CodeAborted.
	_, err = executionapp.CreateBatch([]string{id}, owner, "idem-spec-check-1", &execv1.ExecutionSpec{Command: "echo different"})
	require.ErrorIs(t, err, executionapp.ErrIdempotencyConflict)
	require.Equal(t, connect.CodeAborted, connect.CodeOf(executionError(err)))
}

func TestExecutionAcceptsWebDefaultTimeoutAndOutputSpec(t *testing.T) {
	ctx, owner, _ := actionFixture(t)
	id := rootPolicyNode(t, false)
	// Matches komari-web's DEFAULT_EXECUTION_TIMEOUT_SECONDS (300s) and DEFAULT_EXECUTION_MAX_OUTPUT_BYTES (1 MiB).
	targets := credentialaction.ExecutionTargets([]string{id}, "uname -a", nil, nil, "", 300, 1<<20, false)
	token := actionAuthorization(t, ctx, credential.SensitiveOperation, credentialaction.ScopedResource("execution:create", targets...))

	resp, err := (&executionService{}).CreateExecution(ctx, connect.NewRequest(&execv1.CreateExecutionRequest{
		AgentIds:       []string{id},
		Command:        "uname -a",
		Timeout:        durationpb.New(5 * time.Minute),
		MaxOutputBytes: 1 << 20,
		IdempotencyKey: "web-defaults-check-1",
		TwoFactor:      &commonv1.TwoFactorProof{AuthorizationToken: token},
	}))
	require.NoError(t, err)
	task, err := executionapp.GetOwned(resp.Msg.Execution.ExecutionId, owner)
	require.NoError(t, err)
	require.Equal(t, "uname -a", task.Spec.Command)
	_, _ = executionapp.Cancel(resp.Msg.Execution.ExecutionId, "test cleanup")
}

func TestExecutionValidatesSpecBeforeConsumingAuthorization(t *testing.T) {
	ctx, _, _ := actionFixture(t)
	id := rootPolicyNode(t, false)
	targets := credentialaction.ExecutionTargets([]string{id}, "echo ok", nil, nil, "", 300, 1<<20, false)
	token := actionAuthorization(t, ctx, credential.SensitiveOperation, credentialaction.ScopedResource("execution:create", targets...))

	// Invalid specs (oversized command, sub-second timeout, fractional timeout,
	// oversized output cap) must fail with InvalidArgument BEFORE consuming the ticket.
	invalidRequests := []*execv1.CreateExecutionRequest{
		{
			AgentIds:  []string{id},
			Command:   strings.Repeat("a", 5000),
			TwoFactor: &commonv1.TwoFactorProof{AuthorizationToken: token},
		},
		{
			AgentIds:  []string{id},
			Command:   "echo ok",
			Timeout:   durationpb.New(500 * time.Millisecond),
			TwoFactor: &commonv1.TwoFactorProof{AuthorizationToken: token},
		},
		{
			AgentIds:  []string{id},
			Command:   "echo ok",
			Timeout:   durationpb.New(1500 * time.Millisecond),
			TwoFactor: &commonv1.TwoFactorProof{AuthorizationToken: token},
		},
		{
			AgentIds:       []string{id},
			Command:        "echo ok",
			MaxOutputBytes: 64 << 20,
			TwoFactor:      &commonv1.TwoFactorProof{AuthorizationToken: token},
		},
	}
	for _, invalidReq := range invalidRequests {
		_, err := (&executionService{}).CreateExecution(ctx, connect.NewRequest(invalidReq))
		require.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(err))
	}

	// Because none of the invalid requests consumed the single-use ticket, the
	// matching valid execution request still succeeds.
	resp, err := (&executionService{}).CreateExecution(ctx, connect.NewRequest(&execv1.CreateExecutionRequest{
		AgentIds:       []string{id},
		Command:        "echo ok",
		Timeout:        durationpb.New(5 * time.Minute),
		MaxOutputBytes: 1 << 20,
		TwoFactor:      &commonv1.TwoFactorProof{AuthorizationToken: token},
	}))
	require.NoError(t, err)
	_, _ = executionapp.Cancel(resp.Msg.Execution.ExecutionId, "test cleanup")
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
