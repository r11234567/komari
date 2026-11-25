package connectapi

import (
	"context"
	"errors"
	"strings"

	"connectrpc.com/connect"
	"github.com/komari-monitor/komari/database/clients"
	agent_runtime "github.com/komari-monitor/komari/web/agent"
	deploymentapp "github.com/komari-monitor/komari/web/deployment"
	"github.com/komari-monitor/komari/web/rescueapp"
	deploymentv1 "github.com/r11234567/komari-proto/gen/go/komari/deployment/v1"
	deploymentv1connect "github.com/r11234567/komari-proto/gen/go/komari/deployment/v1/deploymentv1connect"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type deploymentService struct {
	deploymentv1connect.UnimplementedDeploymentServiceHandler
}

// GenerateInstallCommand is retained for callers using the old protocol.
func (s *deploymentService) GenerateInstallCommand(context.Context, *connect.Request[deploymentv1.GenerateInstallCommandRequest]) (*connect.Response[deploymentv1.GenerateInstallCommandResponse], error) {
	return nil, connectError(connect.CodeUnimplemented, errors.New("install commands with one-time tokens have been retired; install the Agent and enroll using browser approval"))
}

func (s *deploymentService) GetDeployment(_ context.Context, req *connect.Request[deploymentv1.GetDeploymentRequest]) (*connect.Response[deploymentv1.GetDeploymentResponse], error) {
	if strings.TrimSpace(req.Msg.AgentId) == "" {
		return nil, connectError(connect.CodeInvalidArgument, errors.New("agent ID is required"))
	}
	profile, _, state, err := clients.GetDeploymentProfileWithDelivery(req.Msg.AgentId)
	if err != nil {
		return nil, connectError(connect.CodeNotFound, errors.New("agent not found"))
	}
	rescueStatus, err := rescueapp.GetStatus(req.Msg.AgentId)
	if err != nil {
		return nil, connectError(connect.CodeInternal, err)
	}
	if state.Revision > state.AppliedRevision && agent_runtime.IsAgentOnline(req.Msg.AgentId) && agent_runtime.IsV2ConfigUpgradeRequired(req.Msg.AgentId) {
		state.Status = clients.DeploymentDeliveryUpgradeRequired
		state.Error = "this Agent does not declare agent.config support; reinstall a Connect-compatible Agent"
	}
	observation, receivedAt := agent_runtime.GetConnectInstallation(req.Msg.AgentId)
	response := &deploymentv1.GetDeploymentResponse{
		Profile:      deploymentToProto(req.Msg.AgentId, profile),
		Delivery:     deliveryToProto(state),
		RescueHelper: rescueStatus,
		Installation: observation,
	}
	if !receivedAt.IsZero() {
		response.InstallationReceivedAt = timestamppb.New(receivedAt)
	}
	return connect.NewResponse(response), nil
}

func (s *deploymentService) SaveDeploymentProfile(_ context.Context, req *connect.Request[deploymentv1.SaveDeploymentProfileRequest]) (*connect.Response[deploymentv1.SaveDeploymentProfileResponse], error) {
	if strings.TrimSpace(req.Msg.AgentId) == "" || req.Msg.Profile == nil {
		return nil, connectError(connect.CodeInvalidArgument, errors.New("agent ID and profile are required"))
	}
	profile, _, state, err := clients.GetDeploymentProfileWithDelivery(req.Msg.AgentId)
	if err != nil {
		return nil, connectError(connect.CodeNotFound, errors.New("agent not found"))
	}
	if req.Msg.ExpectedRevision != state.Revision {
		return nil, connectError(connect.CodeAborted, errors.New("deployment revision conflict"))
	}
	if install := req.Msg.Profile.Install; install != nil {
		profile.Platform = platformFromProto(install.Platform)
		profile.EnableCustomDir = strings.TrimSpace(install.InstallDirectory) != ""
		profile.Dir = install.InstallDirectory
		profile.EnableCustomServiceName = strings.TrimSpace(install.ServiceName) != ""
		profile.ServiceName = install.ServiceName
		profile.DisableAutoUpdate = install.DisableAutoUpdate
		profile.IgnoreUnsafeCert = install.IgnoreUnsafeCertificate
		profile.EnableGPU = install.EnableGpu
		profile.RemoteControlEnabled = install.RemoteControlEnabled
		// disable_web_ssh is a deprecated wire-compatibility field. The Agent
		// treats it exactly like disabling all remote control, so never expose
		// it as an independent installation choice.
		if install.DisableWebSsh {
			profile.RemoteControlEnabled = false
		}
		profile.GetIPAddrFromNIC = install.GetIpAddressFromNic
		profile.EnableGHProxy = install.EnableGithubProxy
		profile.GHProxy = install.GithubProxy
		profile.RuntimeIdentity = runtimeIdentityFromProto(install.RuntimeIdentity)
		if install.Rescue != nil {
			profile.RescueEnabled = install.Rescue.Enabled
		}
	}
	if err := applyRuntime(&profile, req.Msg.Profile.Runtime); err != nil {
		return nil, connectError(connect.CodeInvalidArgument, err)
	}
	result, err := deploymentapp.Save(req.Msg.AgentId, profile, req.Msg.ForceDispatch)
	if err != nil {
		return nil, connectError(connect.CodeInvalidArgument, err)
	}
	return connect.NewResponse(&deploymentv1.SaveDeploymentProfileResponse{
		Profile:  deploymentToProto(req.Msg.AgentId, result.Profile),
		Delivery: deliveryToProto(result.Delivery),
	}), nil
}

func platformFromProto(platform deploymentv1.Platform) string {
	switch platform {
	case deploymentv1.Platform_PLATFORM_WINDOWS_AMD64, deploymentv1.Platform_PLATFORM_WINDOWS_386:
		return "windows"
	case deploymentv1.Platform_PLATFORM_DARWIN_AMD64, deploymentv1.Platform_PLATFORM_DARWIN_ARM64:
		return "macos"
	case deploymentv1.Platform_PLATFORM_LINUX_AMD64, deploymentv1.Platform_PLATFORM_LINUX_ARM64, deploymentv1.Platform_PLATFORM_LINUX_386:
		return "linux"
	default:
		return "linux"
	}
}
