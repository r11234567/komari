package connectapi

import (
	"connectrpc.com/connect"
	"context"
	"github.com/komari-monitor/komari/database/clients"
	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/database/privilegeddelivery"
	"github.com/komari-monitor/komari/pkg/rpc"
	configv1 "github.com/r11234567/komari-proto/gen/go/komari/config/v1"
	hostv1 "github.com/r11234567/komari-proto/gen/go/komari/host/v1"
	reportv1 "github.com/r11234567/komari-proto/gen/go/komari/report/v1"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
	"strings"
	"testing"
	"time"
)

// Uses actual node creation, unlike the old fixture that manually inserted r0.
func TestRealNodeCreationInitializesBothTracksAndRoundTripsRuntime(t *testing.T) {
	for _, named := range []bool{false, true} {
		t.Run(map[bool]string{false: "unnamed", true: "named"}[named], func(t *testing.T) {
			var id string
			var err error
			if named {
				id, _, err = clients.CreateClientWithName("fresh baseline")
			} else {
				id, _, err = clients.CreateClient()
			}
			require.NoError(t, err)
			_, _, before, err := clients.GetDeploymentProfileWithDelivery(id)
			require.NoError(t, err)
			require.Zero(t, before.Revision)
			ctx := rpc.NewContextWithMeta(context.Background(), &rpc.ContextMeta{Principal: rpc.NewAgentPrincipal(id)})
			_, err = (&hostService{}).NegotiateHostProtocol(ctx, connect.NewRequest(&hostv1.NegotiateHostProtocolRequest{AgentId: id, ServiceId: "komari-agent", StartupId: "new-boot", PrivilegeMode: 2, ProtocolVersion: 2}))
			require.NoError(t, err)
			no := false
			yes := true
			day := uint32(7)
			runtime := &configv1.RuntimeConfig{MemoryIncludeCache: &no, DetailedGpu: &no, GetIpAddrFromNic: &yes, ReportInterval: durationpb.New(30 * time.Second), TrafficResetDay: &day}
			request := &configv1.InitializeNodeConfigRequest{AgentId: id, ServiceId: "komari-agent", PolicyHash: strings.Repeat("a", 64), Runtime: runtime, Privileged: &configv1.PrivilegedConfig{RequiredPrivilegeMode: reportv1.PrivilegeMode_PRIVILEGE_MODE_LINUX_NON_ROOT}}
			service := &configService{}
			response, err := service.InitializeNodeConfig(ctx, connect.NewRequest(request))
			require.NoError(t, err)
			require.True(t, response.Msg.Initialized, response.Msg.SkippedReason)
			require.EqualValues(t, 1, response.Msg.Runtime.Revision)
			require.EqualValues(t, 1, response.Msg.Privileged.Revision)
			require.NotNil(t, response.Msg.Privileged.Signature)
			desired, err := service.GetDesiredConfig(ctx, connect.NewRequest(&configv1.GetDesiredConfigRequest{AgentId: id}))
			require.NoError(t, err)
			require.NotNil(t, desired.Msg.Desired)
			require.True(t, proto.Equal(runtime, desired.Msg.Desired.Runtime), "baseline and ordinary channel disagree: %v vs %v", runtime, desired.Msg.Desired.Runtime)
			state, err := privilegeddelivery.GetStateWithDB(dbcore.GetDBInstance(), id)
			require.NoError(t, err)
			require.Zero(t, state.State.AppliedRevision, "baseline creation is not a daemon ACK")
			// A new startup checks the same ordinary revision and generates a fresh ACK.
			_, err = service.AcknowledgeConfig(ctx, connect.NewRequest(&configv1.AcknowledgeConfigRequest{AgentId: id, Revision: 1, Status: configv1.ConfigApplyStatus_CONFIG_APPLY_STATUS_APPLIED}))
			require.NoError(t, err)
			old := time.Now().Add(-48 * time.Hour).UTC()
			require.NoError(t, dbcore.GetDBInstance().Model(&models.ClientDeploymentProfile{}).Where("client = ?", id).Update("finished_at", old).Error)
			_, err = service.AcknowledgeConfig(ctx, connect.NewRequest(&configv1.AcknowledgeConfigRequest{AgentId: id, Revision: 1, Status: configv1.ConfigApplyStatus_CONFIG_APPLY_STATUS_APPLIED, FinishedAt: timestamppb.New(old)}))
			require.NoError(t, err)
			_, _, after, err := clients.GetDeploymentProfileWithDelivery(id)
			require.NoError(t, err)
			require.True(t, after.FinishedAt.After(old.Add(time.Hour)), "fresh ACK must use server receipt time")
		})
	}
}
