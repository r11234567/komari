package connectapi

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/komari-monitor/komari/database/clients"
	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/internal/credentialaction"
	credential "github.com/komari-monitor/komari/internal/trustedcredential"
	"github.com/komari-monitor/komari/web/rescueapp"
	commonv1 "github.com/r11234567/komari-proto/gen/go/komari/common/v1"
	execv1 "github.com/r11234567/komari-proto/gen/go/komari/exec/v1"
	rescuev1 "github.com/r11234567/komari-proto/gen/go/komari/rescue/v1"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestRescueSpecValidationIdempotencyAndBinding(t *testing.T) {
	id, _, err := clients.CreateClient()
	require.NoError(t, err)
	profile, _, err := clients.GetDeploymentProfile(id)
	require.NoError(t, err)
	profile.RescueEnabled = true
	_, err = clients.SaveDeploymentProfile(id, profile)
	require.NoError(t, err)
	const key = "test-enrollment"
	db := dbcore.GetDBInstance()
	require.NoError(t, db.Create(&models.AgentCredential{Client: id, AgentKeyID: key}).Error)
	t.Cleanup(func() { db.Where("client = ?", id).Delete(&models.AgentCredential{}) })
	status := &rescuev1.RescueHelperStatus{
		Installed: true, GuardianRunning: true, HelperRunning: true, HelperInstanceId: "test-helper",
	}
	require.NoError(t, rescueapp.ReportStatus(id, status))

	svc := &rescueService{}
	const action = rescuev1.RescueAction_RESCUE_ACTION_TEMPORARY_SSH_ACCESS
	const source = "192.0.2.6"
	proofFor := func(port uint32, ip string) (context.Context, string) {
		t.Helper()
		ctx, _, _ := actionFixture(t)
		targets := credentialaction.RescueTargets(id, int32(action), nil, 300, 256*1024, port, ip, "", key)
		return ctx, actionAuthorization(t, ctx, credential.SensitiveOperation, credentialaction.ScopedResource("rescue:create", targets...))
	}
	request := func(port uint32, ip, idemKey, token string) *connect.Request[rescuev1.CreateRescueSessionRequest] {
		return connect.NewRequest(&rescuev1.CreateRescueSessionRequest{
			AgentId: id, Action: action, SshPort: proto.Uint32(port),
			SshWindow: &rescuev1.SSHWindowRequest{SourceIp: ip, EnrollmentKeyId: key},
			Timeout:   durationpb.New(300 * time.Second), MaxOutputBytes: 256 * 1024,
			IdempotencyKey: idemKey, TwoFactor: &commonv1.TwoFactorProof{AuthorizationToken: token},
		})
	}
	ctx, token := proofFor(2222, source)
	_, err = svc.CreateRescueSession(ctx, request(2222, source, "", token))
	require.Equal(t, connect.CodeFailedPrecondition, connect.CodeOf(err))
	status.SshWindowSupported = true
	status.SshAccessPort = 2222
	status.SshEnrollmentKeyId = key
	require.NoError(t, rescueapp.ReportStatus(id, status))
	ctx, token = proofFor(2200, source)
	_, err = svc.CreateRescueSession(ctx, request(2200, source, "", token))
	require.Equal(t, connect.CodeFailedPrecondition, connect.CodeOf(err))
	ctx, token = proofFor(2222, source)

	// Invalid SSH port on a non-SSH action must fail before consuming the valid proof.
	_, err = svc.CreateRescueSession(ctx, connect.NewRequest(&rescuev1.CreateRescueSessionRequest{
		AgentId:        id,
		Action:         rescuev1.RescueAction_RESCUE_ACTION_REBOOT,
		SshPort:        proto.Uint32(2222),
		Timeout:        durationpb.New(300 * time.Second),
		MaxOutputBytes: 256 * 1024,
		TwoFactor:      &commonv1.TwoFactorProof{AuthorizationToken: token},
	}))
	require.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(err))

	// A proof cannot open the port to a different source address.
	_, err = svc.CreateRescueSession(ctx, request(2222, "192.0.2.7", "", token))
	require.Equal(t, connect.CodeNotFound, connect.CodeOf(err))

	// Same token must still succeed for the exact authorized specification because the invalid request did not burn it.
	idemKey := "idem-" + uuid.NewString()
	created, err := svc.CreateRescueSession(ctx, request(2222, source, idemKey, token))
	require.NoError(t, err)
	require.Equal(t, uint32(2222), created.Msg.Session.GetSshPort())

	// Reusing the same idempotency key with different parameters must abort.
	ctx, token = proofFor(2222, source)
	retry, err := svc.CreateRescueSession(ctx, request(2222, source, idemKey, token))
	require.NoError(t, err)
	require.Equal(t, created.Msg.Session.SessionId, retry.Msg.Session.SessionId)
	otherSource := "2001:db8::7"
	ctx, token = proofFor(2222, otherSource)
	_, err = svc.CreateRescueSession(ctx, request(2222, otherSource, idemKey, token))
	require.Equal(t, connect.CodeAborted, connect.CodeOf(err))
	require.ErrorIs(t, err, rescueapp.ErrIdempotencyConflict)

	grantID := created.Msg.Session.SessionId
	assignment, err := rescueapp.NextAssignment(id, status.HelperInstanceId, "")
	require.NoError(t, err)
	require.Equal(t, grantID, assignment.GetAssignmentId())
	require.Equal(t, source, assignment.GetSession().GetSshWindow().GetSourceIp())
	// The helper's periodic kernel receipt recovers a missing final RPC.
	start := time.Now().UTC()
	status.SshAccess = &rescuev1.TemporarySSHAccess{
		Port: 2222, Granted: true, SourceIp: source, GrantId: grantID,
		GrantedAt: timestamppb.New(start), ExpiresAt: timestamppb.New(start.Add(15 * time.Minute)),
		ExpiryMechanism: rescuev1.ExpiryMechanism_EXPIRY_MECHANISM_NFTABLES_TIMEOUT,
	}
	require.NoError(t, rescueapp.ReportStatus(id, status))
	granted, err := rescueapp.GetSession(grantID)
	require.NoError(t, err)
	require.Equal(t, commonv1.OperationState_OPERATION_STATE_SUCCEEDED, granted.State)
	// An exact revocation can be queued while the helper is disconnected.
	require.NoError(t, db.Model(&models.ClientRescueHelper{}).Where("client = ?", id).
		UpdateColumn("updated_at", start.Add(-time.Hour)).Error)
	const revoke = rescuev1.RescueAction_RESCUE_ACTION_REVOKE_TEMPORARY_SSH_ACCESS
	ctx, _, _ = actionFixture(t)
	targets := credentialaction.RescueTargets(id, int32(revoke), nil, 300, 256*1024, 0, "", grantID, key)
	token = actionAuthorization(t, ctx, credential.SensitiveOperation, credentialaction.ScopedResource("rescue:create", targets...))
	queued, err := svc.CreateRescueSession(ctx, connect.NewRequest(&rescuev1.CreateRescueSessionRequest{
		AgentId: id, Action: revoke, SshWindow: &rescuev1.SSHWindowRequest{PreviousGrantId: grantID, EnrollmentKeyId: key},
		Timeout: durationpb.New(300 * time.Second), MaxOutputBytes: 256 * 1024,
		TwoFactor: &commonv1.TwoFactorProof{AuthorizationToken: token},
	}))
	require.NoError(t, err)
	offline, err := rescueapp.GetStatus(id)
	require.NoError(t, err)
	require.False(t, offline.GetHelperRunning())
	require.True(t, offline.GetSshRevokePending())
	require.Equal(t, grantID, offline.GetSshAccess().GetGrantId())
	require.NoError(t, rescueapp.ReportStatus(id, status))
	assignment, err = rescueapp.NextAssignment(id, status.HelperInstanceId, grantID)
	require.NoError(t, err)
	require.Equal(t, queued.Msg.GetSession().GetSessionId(), assignment.GetAssignmentId())
	require.Equal(t, grantID, assignment.GetSession().GetSshWindow().GetPreviousGrantId())
	status.SshAccess.Granted = false
	require.NoError(t, rescueapp.ReportStatus(id, status))
	closed, err := rescueapp.GetStatus(id)
	require.NoError(t, err)
	require.False(t, closed.GetSshRevokePending())
	require.Equal(t, grantID, closed.GetSshAccess().GetGrantId())
	require.False(t, closed.GetSshAccess().GetGranted())

	// CancelExecution must reject unknown execution IDs before consuming the proof.
	cancelCtx, _, _ := actionFixture(t)
	cancelExec := &executionService{}
	cancelToken := actionAuthorization(t, cancelCtx, credential.SensitiveOperation, credentialaction.ScopedResource("execution:cancel", "missing-exec"))
	_, err = cancelExec.CancelExecution(cancelCtx, connect.NewRequest(&execv1.CancelExecutionRequest{
		ExecutionId: "missing-exec",
		TwoFactor:   &commonv1.TwoFactorProof{AuthorizationToken: cancelToken},
	}))
	require.Equal(t, connect.CodeNotFound, connect.CodeOf(err))
}
