package connectapi

import (
	"context"
	"errors"
	"strings"

	"connectrpc.com/connect"
	"github.com/komari-monitor/komari/database/clients"
	deploymentv1 "github.com/r11234567/komari-proto/gen/go/komari/deployment/v1"
	deploymentv1connect "github.com/r11234567/komari-proto/gen/go/komari/deployment/v1/deploymentv1connect"
)

type deploymentService struct {
	deploymentv1connect.UnimplementedDeploymentServiceHandler
}

func (s *deploymentService) GetDeployment(_ context.Context, req *connect.Request[deploymentv1.GetDeploymentRequest]) (*connect.Response[deploymentv1.GetDeploymentResponse], error) {
	if strings.TrimSpace(req.Msg.AgentId) == "" {
		return nil, connectError(connect.CodeInvalidArgument, errors.New("agent ID is required"))
	}
	profile, _, state, err := clients.GetDeploymentProfileWithDelivery(req.Msg.AgentId)
	if err != nil {
		return nil, connectError(connect.CodeNotFound, errors.New("agent not found"))
	}
	return connect.NewResponse(&deploymentv1.GetDeploymentResponse{
		Profile:  deploymentToProto(req.Msg.AgentId, profile),
		Delivery: deliveryToProto(state),
	}), nil
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
		profile.EnableGHProxy = install.EnableGithubProxy
		profile.GHProxy = install.GithubProxy
	}
	if err := applyRuntime(&profile, req.Msg.Profile.Runtime); err != nil {
		return nil, connectError(connect.CodeInvalidArgument, err)
	}
	stored, delivery, _, err := clients.SaveDeploymentProfileForDispatch(req.Msg.AgentId, profile)
	if err != nil {
		return nil, connectError(connect.CodeInvalidArgument, err)
	}
	return connect.NewResponse(&deploymentv1.SaveDeploymentProfileResponse{
		Profile:  deploymentToProto(req.Msg.AgentId, stored),
		Delivery: deliveryToProto(delivery),
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
