package connectapi

import (
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/komari-monitor/komari/database/clients"
	"github.com/komari-monitor/komari/internal/credentialaction"
	credential "github.com/komari-monitor/komari/internal/trustedcredential"
	"github.com/komari-monitor/komari/web/rescueapp"
	commonv1 "github.com/r11234567/komari-proto/gen/go/komari/common/v1"
	execv1 "github.com/r11234567/komari-proto/gen/go/komari/exec/v1"
	rescuev1 "github.com/r11234567/komari-proto/gen/go/komari/rescue/v1"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
)

func TestRescueSpecValidationIdempotencyAndBinding(t *testing.T) {
	ctx, _, _ := actionFixture(t)
	id, _, err := clients.CreateClient()
	require.NoError(t, err)
	profile, _, err := clients.GetDeploymentProfile(id)
	require.NoError(t, err)
	profile.RescueEnabled = true
	_, err = clients.SaveDeploymentProfile(id, profile)
	require.NoError(t, err)

	svc := &rescueService{}
	targets := credentialaction.RescueTargets(id, int32(rescuev1.RescueAction_RESCUE_ACTION_TEMPORARY_SSH_ACCESS), nil, 300, 256*1024, 2222)
	token := actionAuthorization(t, ctx, credential.SensitiveOperation, credentialaction.ScopedResource("rescue:create", targets...))

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

	// Parameter mismatch against the proof targets must fail.
	_, err = svc.CreateRescueSession(ctx, connect.NewRequest(&rescuev1.CreateRescueSessionRequest{
		AgentId:        id,
		Action:         rescuev1.RescueAction_RESCUE_ACTION_TEMPORARY_SSH_ACCESS,
		SshPort:        proto.Uint32(2222),
		Timeout:        durationpb.New(600 * time.Second),
		MaxOutputBytes: 256 * 1024,
		TwoFactor:      &commonv1.TwoFactorProof{AuthorizationToken: token},
	}))
	require.Equal(t, connect.CodePermissionDenied, connect.CodeOf(err))

	// Same token must still succeed for the exact authorized specification because the invalid request did not burn it.
	idemKey := "idem-" + uuid.NewString()
	created, err := svc.CreateRescueSession(ctx, connect.NewRequest(&rescuev1.CreateRescueSessionRequest{
		AgentId:        id,
		Action:         rescuev1.RescueAction_RESCUE_ACTION_TEMPORARY_SSH_ACCESS,
		SshPort:        proto.Uint32(2222),
		Timeout:        durationpb.New(300 * time.Second),
		MaxOutputBytes: 256 * 1024,
		IdempotencyKey: idemKey,
		TwoFactor:      &commonv1.TwoFactorProof{AuthorizationToken: token},
	}))
	require.NoError(t, err)
	require.Equal(t, uint32(2222), created.Msg.Session.GetSshPort())

	// Reusing the same idempotency key with different parameters must abort.
	otherTargets := credentialaction.RescueTargets(id, int32(rescuev1.RescueAction_RESCUE_ACTION_TEMPORARY_SSH_ACCESS), nil, 300, 256*1024, 2200)
	otherToken := actionAuthorization(t, ctx, credential.SensitiveOperation, credentialaction.ScopedResource("rescue:create", otherTargets...))
	_, err = svc.CreateRescueSession(ctx, connect.NewRequest(&rescuev1.CreateRescueSessionRequest{
		AgentId:        id,
		Action:         rescuev1.RescueAction_RESCUE_ACTION_TEMPORARY_SSH_ACCESS,
		SshPort:        proto.Uint32(2200),
		Timeout:        durationpb.New(300 * time.Second),
		MaxOutputBytes: 256 * 1024,
		IdempotencyKey: idemKey,
		TwoFactor:      &commonv1.TwoFactorProof{AuthorizationToken: otherToken},
	}))
	require.Equal(t, connect.CodeAborted, connect.CodeOf(err))
	require.ErrorIs(t, err, rescueapp.ErrIdempotencyConflict)

	// CancelExecution must reject unknown execution IDs before consuming the proof.
	cancelExec := &executionService{}
	cancelToken := actionAuthorization(t, ctx, credential.SensitiveOperation, credentialaction.ScopedResource("execution:cancel", "missing-exec"))
	_, err = cancelExec.CancelExecution(ctx, connect.NewRequest(&execv1.CancelExecutionRequest{
		ExecutionId: "missing-exec",
		TwoFactor:   &commonv1.TwoFactorProof{AuthorizationToken: cancelToken},
	}))
	require.Equal(t, connect.CodeNotFound, connect.CodeOf(err))
}
