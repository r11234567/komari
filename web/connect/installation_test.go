package connectapi

import (
	"context"
	"testing"

	"connectrpc.com/connect"
	"github.com/komari-monitor/komari/database/clients"
	"github.com/komari-monitor/komari/pkg/rpc"
	agentRuntime "github.com/komari-monitor/komari/web/agent"
	browserv1 "github.com/r11234567/komari-proto/gen/go/komari/browser/v1"
	configv1 "github.com/r11234567/komari-proto/gen/go/komari/config/v1"
	deploymentv1 "github.com/r11234567/komari-proto/gen/go/komari/deployment/v1"
	reportv1 "github.com/r11234567/komari-proto/gen/go/komari/report/v1"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestDaemonObservationSurvivesReportToAdminWithoutTemplateOrPublicLeak(t *testing.T) {
	id, _, err := clients.CreateClientWithName("installation observation")
	require.NoError(t, err)
	t.Cleanup(func() { agentRuntime.DeleteConnectedClients(id) })
	agentCtx := rpc.NewContextWithMeta(context.Background(), &rpc.ContextMeta{Principal: rpc.NewAgentPrincipal(id)})
	// A saved Linux template must not replace the reporting Windows daemon.
	profile, _, _, err := clients.GetDeploymentProfileWithDelivery(id)
	require.NoError(t, err)
	profile.Platform, profile.Dir, profile.ServiceName = "linux", "/template/not/actual", "template.service"
	_, delivery, _, err := clients.SaveDeploymentProfileForDispatchForced(id, profile, true)
	require.NoError(t, err)
	accepted, err := clients.CompleteDeploymentConfigDelivery(id, delivery.Revision, clients.DeploymentDeliveryRejected, "same runtime revision has different content")
	require.NoError(t, err)
	require.True(t, accepted)
	no := false
	observation := &reportv1.InstallationObservation{StartupId: "windows-boot", ObservedAt: timestamppb.Now(), OperatingSystem: "windows", Executable: `C:\Program Files\Komari\komari-agent.exe`, ServiceName: "komari-agent", ProcessUser: `NT AUTHORITY\LOCAL SERVICE`, IgnoreUnsafeCertificate: &no}
	_, err = (&reportService{}).SubmitReport(agentCtx, connect.NewRequest(&reportv1.SubmitReportRequest{Report: &reportv1.AgentReport{AgentId: id, Sequence: 1, Metadata: &reportv1.AgentMetadata{Installation: observation, AppliedConfigRevision: delivery.Revision}}}))
	require.NoError(t, err)
	response, err := (&deploymentService{}).GetDeployment(context.Background(), connect.NewRequest(&deploymentv1.GetDeploymentRequest{AgentId: id}))
	require.NoError(t, err)
	require.Equal(t, observation.Executable, response.Msg.Installation.Executable)
	require.Equal(t, observation.ProcessUser, response.Msg.Installation.ProcessUser)
	require.NotNil(t, response.Msg.InstallationReceivedAt)
	_, _, after, err := clients.GetDeploymentProfileWithDelivery(id)
	require.NoError(t, err)
	require.Equal(t, clients.DeploymentDeliveryRejected, after.Status, "a numeric revision report cannot acknowledge different content")
	require.Zero(t, after.AppliedRevision, "unverified disk revision became a confirmed application")
	require.Equal(t, "same runtime revision has different content", after.Error)
	public, err := (&browserService{}).GetAgent(context.Background(), connect.NewRequest(&browserv1.GetAgentRequest{AgentId: id}))
	require.NoError(t, err)
	require.Nil(t, public.Msg.GetLatestReport().GetMetadata().GetInstallation(), "local account and paths must remain administrator-only")
	_, err = (&reportService{}).SubmitReport(agentCtx, connect.NewRequest(&reportv1.SubmitReportRequest{Report: &reportv1.AgentReport{AgentId: id, Sequence: 2, Metadata: &reportv1.AgentMetadata{}}}))
	require.NoError(t, err)
	legacy, err := (&deploymentService{}).GetDeployment(context.Background(), connect.NewRequest(&deploymentv1.GetDeploymentRequest{AgentId: id}))
	require.NoError(t, err)
	require.Nil(t, legacy.Msg.Installation, "missing report must stay unknown, not fall back to template")
	// Re-dispatch creates a new content version while leaving the install
	// template alone. This is how an administrator resolves a same-rN conflict.
	saved, err := (&deploymentService{}).SaveDeploymentProfile(context.Background(), connect.NewRequest(&deploymentv1.SaveDeploymentProfileRequest{
		AgentId: id, ExpectedRevision: delivery.Revision, ForceDispatch: true,
		Profile: &deploymentv1.DeploymentProfile{AgentId: id, Runtime: &configv1.RuntimeConfig{GetIpAddrFromNic: &no}},
	}))
	require.NoError(t, err)
	require.Greater(t, saved.Msg.Delivery.DesiredRevision, delivery.Revision)
	stored, _, err := clients.GetDeploymentProfile(id)
	require.NoError(t, err)
	require.Equal(t, "/template/not/actual", stored.Dir)
	require.Equal(t, "template.service", stored.ServiceName)
	require.False(t, stored.GetIPAddrFromNIC)
	_, err = (&configService{}).AcknowledgeConfig(agentCtx, connect.NewRequest(&configv1.AcknowledgeConfigRequest{
		AgentId: id, Revision: saved.Msg.Delivery.DesiredRevision, Status: configv1.ConfigApplyStatus_CONFIG_APPLY_STATUS_APPLIED,
	}))
	require.NoError(t, err)
	_, _, completed, err := clients.GetDeploymentProfileWithDelivery(id)
	require.NoError(t, err)
	require.Equal(t, clients.DeploymentDeliveryApplied, completed.Status)
	require.Equal(t, saved.Msg.Delivery.DesiredRevision, completed.AppliedRevision)
}

func TestInstallationObservationQueryRequiresAdmin(t *testing.T) {
	policy, exists := newPolicyInterceptor().policies["/komari.deployment.v1.DeploymentService/GetDeployment"]
	require.True(t, exists)
	require.Equal(t, rpc.RoleAdmin, policy.role, "local paths and process accounts are administrator-only")
}
