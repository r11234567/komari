package connectapi

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/komari-monitor/komari/database/clients"
	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/pkg/rpc"
	"github.com/komari-monitor/komari/web/rescueapp"
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
	require.NoError(t, rescueapp.ReportStatus(id, &rescuev1.RescueHelperStatus{
		Installed: true, GuardianRunning: true, HelperRunning: true, HelperInstanceId: "test-helper",
	}))
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

func TestRescueStatusAndAdmissionRequireRecentHelperReport(t *testing.T) {
	id, _, err := clients.CreateClient()
	require.NoError(t, err)
	profile, _, err := clients.GetDeploymentProfile(id)
	require.NoError(t, err)
	profile.RescueEnabled = true
	_, err = clients.SaveDeploymentProfile(id, profile)
	require.NoError(t, err)

	ctx := rpc.NewContextWithMeta(context.Background(), &rpc.ContextMeta{Principal: rpc.NewUserPrincipal("diagnostic-admin")})
	svc := &rescueService{}
	status := func() *rescuev1.RescueHelperStatus {
		response, err := svc.GetRescueStatus(ctx, connect.NewRequest(&rescuev1.GetRescueStatusRequest{AgentId: id}))
		require.NoError(t, err)
		return response.Msg.Status
	}
	create := func() error {
		_, err := svc.CreateRescueSession(ctx, connect.NewRequest(&rescuev1.CreateRescueSessionRequest{
			AgentId: id, Action: rescuev1.RescueAction_RESCUE_ACTION_DIAGNOSTICS,
		}))
		return err
	}
	require.False(t, status().Installed)
	require.Equal(t, connect.CodeFailedPrecondition, connect.CodeOf(create()))
	require.NoError(t, rescueapp.ReportStatus(id, &rescuev1.RescueHelperStatus{
		Installed: true, GuardianRunning: true, HelperRunning: true, HelperInstanceId: "fresh-helper",
	}))
	require.True(t, status().Installed)
	require.NoError(t, create())

	db := dbcore.GetDBInstance()
	require.NoError(t, db.Model(&models.ClientRescueHelper{}).Where("client = ?", id).
		UpdateColumn("updated_at", time.Now().UTC().Add(-time.Hour)).Error)
	require.True(t, status().Requested)
	require.False(t, status().Installed)
	require.Equal(t, connect.CodeFailedPrecondition, connect.CodeOf(create()))
	_, err = rescueapp.NextAssignment(id, "fresh-helper", "")
	require.Error(t, err)
	var count int64
	require.NoError(t, db.Model(&models.RescueSession{}).Where("client = ?", id).Count(&count).Error)
	require.EqualValues(t, 1, count)
}
