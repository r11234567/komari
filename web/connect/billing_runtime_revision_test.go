package connectapi

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/komari-monitor/komari/database/clients"
	"github.com/komari-monitor/komari/pkg/rpc"
	deploymentapp "github.com/komari-monitor/komari/web/deployment"
	configv1 "github.com/r11234567/komari-proto/gen/go/komari/config/v1"
	"github.com/stretchr/testify/require"
)

func TestBillingResetDayGetsNewRevisionAndReachesConnectWatcher(t *testing.T) {
	id, _, err := clients.CreateClientWithName("billing runtime revision")
	require.NoError(t, err)
	profile, _, err := clients.GetDeploymentProfile(id)
	require.NoError(t, err)
	profile.EnableMonthRotate, profile.MonthRotate = true, 7
	_, first, _, err := clients.SaveDeploymentProfileForDispatchForced(id, profile, true)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	delivered := make(chan clients.DeploymentDeliveryState, 1)
	go func() {
		_, found, state, err := deploymentapp.WaitForDesired(ctx, id, first.Revision)
		if err == nil && found {
			delivered <- state
		}
	}()
	require.NoError(t, clients.SaveClient(map[string]interface{}{"uuid": id, "traffic_reset_day": float64(17)}))
	_, err = deploymentapp.DispatchCurrent(id)
	require.NoError(t, err)
	select {
	case state := <-delivered:
		require.Equal(t, first.Revision+1, state.Revision)
	case <-ctx.Done():
		t.Fatal("billing update did not reach the Connect runtime watcher")
	}
	agentCtx := rpc.NewContextWithMeta(context.Background(), &rpc.ContextMeta{Principal: rpc.NewAgentPrincipal(id)})
	response, err := (&configService{}).GetDesiredConfig(agentCtx, connect.NewRequest(&configv1.GetDesiredConfigRequest{AgentId: id, AppliedRevision: first.Revision}))
	require.NoError(t, err)
	require.NotNil(t, response.Msg.Desired)
	require.EqualValues(t, 17, response.Msg.Desired.Runtime.GetTrafficResetDay())
	require.Equal(t, first.Revision+1, response.Msg.Desired.Revision)
}
