package connectapi

import (
	"context"
	"testing"

	"connectrpc.com/connect"
	"github.com/komari-monitor/komari/database/clients"
	"github.com/komari-monitor/komari/pkg/rpc"
	rescuev1 "github.com/r11234567/komari-proto/gen/go/komari/rescue/v1"
	"github.com/stretchr/testify/require"
)

func TestMachineAndPerformanceDiagnosticsRequireAdministratorButNotTwoFactor(t *testing.T) {
	id, _, err := clients.CreateClient()
	require.NoError(t, err)
	profile, _, err := clients.GetDeploymentProfile(id)
	require.NoError(t, err)
	profile.RescueEnabled = true
	_, err = clients.SaveDeploymentProfile(id, profile)
	require.NoError(t, err)
	svc := &rescueService{}
	for _, action := range []rescuev1.RescueAction{
		rescuev1.RescueAction_RESCUE_ACTION_DIAGNOSTICS,
		rescuev1.RescueAction_RESCUE_ACTION_DETAILED_CPU_METRICS,
		rescuev1.RescueAction_RESCUE_ACTION_DETAILED_MEMORY_METRICS,
	} {
		t.Run(action.String(), func(t *testing.T) {
			request := func() *connect.Request[rescuev1.CreateRescueSessionRequest] {
				return connect.NewRequest(&rescuev1.CreateRescueSessionRequest{AgentId: id, Action: action})
			}
			_, err := svc.CreateRescueSession(context.Background(), request())
			require.Equal(t, connect.CodePermissionDenied, connect.CodeOf(err))
			agent := rpc.NewContextWithMeta(context.Background(), &rpc.ContextMeta{Principal: rpc.NewAgentPrincipal(id)})
			_, err = svc.CreateRescueSession(agent, request())
			require.Equal(t, connect.CodePermissionDenied, connect.CodeOf(err))
			admin := rpc.NewContextWithMeta(context.Background(), &rpc.ContextMeta{Principal: rpc.NewUserPrincipal("diagnostic-admin")})
			response, err := svc.CreateRescueSession(admin, request())
			require.NoError(t, err)
			require.Equal(t, action, response.Msg.Session.Action)
		})
	}
	admin := rpc.NewContextWithMeta(context.Background(), &rpc.ContextMeta{Principal: rpc.NewUserPrincipal("diagnostic-admin")})
	for _, action := range []rescuev1.RescueAction{
		rescuev1.RescueAction_RESCUE_ACTION_REBOOT,
		rescuev1.RescueAction_RESCUE_ACTION_TEMPORARY_SSH_ACCESS,
		rescuev1.RescueAction_RESCUE_ACTION_RESTORE_NETWORK,
	} {
		_, err := svc.CreateRescueSession(admin, connect.NewRequest(&rescuev1.CreateRescueSessionRequest{AgentId: id, Action: action}))
		require.Equal(t, connect.CodeUnauthenticated, connect.CodeOf(err), action.String())
	}
}
