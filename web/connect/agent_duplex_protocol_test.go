package connectapi

import (
	"context"
	"testing"

	"connectrpc.com/connect"
	"github.com/komari-monitor/komari/database/clients"
	"github.com/komari-monitor/komari/pkg/config"
	"github.com/komari-monitor/komari/pkg/rpc"
	configv1 "github.com/r11234567/komari-proto/gen/go/komari/config/v1"
	"github.com/stretchr/testify/require"
)

func TestAgentDuplexPreferenceOnAuthenticatedConfig(t *testing.T) {
	adminCtx, _, _ := actionFixture(t)
	agentID, _, err := clients.CreateClient()
	require.NoError(t, err)
	req := connect.NewRequest(&configv1.GetDesiredConfigRequest{AgentId: agentID})
	service := &configService{}
	_, err = service.GetDesiredConfig(adminCtx, req)
	require.Error(t, err)
	ctx := rpc.NewContextWithMeta(context.Background(), &rpc.ContextMeta{
		Principal: rpc.NewAgentPrincipal(agentID), ClientUUID: agentID,
	})

	for _, enabled := range []bool{false, true, false} {
		require.NoError(t, config.Set(config.AgentCloudflareProxyKey, enabled))
		result, err := service.GetDesiredConfig(ctx, req)
		require.NoError(t, err)
		want := ""
		if enabled {
			want = "grpc"
		}
		require.Equal(t, want, result.Header().Get(agentDuplexProtocolHeader))
	}
}
