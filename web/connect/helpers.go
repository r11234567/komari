package connectapi

import (
	"errors"
	"math"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/komari-monitor/komari/database/clients"
	"github.com/komari-monitor/komari/pkg/rpc"
	legacyv2 "github.com/komari-monitor/komari/protocol/v2"
	commonv1 "github.com/r11234567/komari-proto/gen/go/komari/common/v1"
	configv1 "github.com/r11234567/komari-proto/gen/go/komari/config/v1"
	deploymentv1 "github.com/r11234567/komari-proto/gen/go/komari/deployment/v1"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func connectError(code connect.Code, err error) error {
	if err == nil {
		err = errors.New(code.String())
	}
	return connect.NewError(code, err)
}

func requireAgent(ctxMeta *rpc.ContextMeta, requested string) (string, error) {
	if ctxMeta == nil || ctxMeta.Principal == nil || ctxMeta.Principal.Type != rpc.PrincipalAgent {
		return "", connectError(connect.CodeUnauthenticated, errors.New("agent authentication is required"))
	}
	authenticated := ctxMeta.Principal.ClientUUID
	if requested != "" && requested != authenticated {
		return "", connectError(connect.CodePermissionDenied, errors.New("agent ID does not match the authenticated agent"))
	}
	return authenticated, nil
}

func deliveryState(status string) commonv1.DeliveryState {
	switch status {
	case clients.DeploymentDeliverySaved:
		return commonv1.DeliveryState_DELIVERY_STATE_SAVED
	case clients.DeploymentDeliverySent:
		return commonv1.DeliveryState_DELIVERY_STATE_SENT
	case clients.DeploymentDeliveryApplied:
		return commonv1.DeliveryState_DELIVERY_STATE_APPLIED
	case clients.DeploymentDeliveryRejected, clients.DeploymentDeliveryFailed:
		return commonv1.DeliveryState_DELIVERY_STATE_REJECTED
	case clients.DeploymentDeliveryOffline:
		return commonv1.DeliveryState_DELIVERY_STATE_OFFLINE
	case clients.DeploymentDeliveryUpgradeRequired:
		return commonv1.DeliveryState_DELIVERY_STATE_UPGRADE_REQUIRED
	default:
		return commonv1.DeliveryState_DELIVERY_STATE_UNSPECIFIED
	}
}

func runtimeToProto(config legacyv2.ConfigParams) *configv1.RuntimeConfig {
	result := &configv1.RuntimeConfig{
		MemoryIncludeCache: config.MemoryIncludeCache,
		DetailedGpu:        config.DetailedGPU,
	}
	if config.IncludeNics != nil {
		result.IncludeNics = splitList(*config.IncludeNics, ",")
	}
	if config.ExcludeNics != nil {
		result.ExcludeNics = splitList(*config.ExcludeNics, ",")
	}
	if config.IncludeMountpoints != nil {
		result.IncludeMountpoints = splitList(*config.IncludeMountpoints, ";")
	}
	if config.Interval != nil {
		result.ReportInterval = durationpb.New(time.Duration(*config.Interval * float64(time.Second)))
	}
	if config.MonthRotate != nil && *config.MonthRotate >= 0 {
		day := uint32(*config.MonthRotate)
		result.TrafficResetDay = &day
	}
	return result
}

func applyRuntime(profile *clients.DeploymentProfile, runtime *configv1.RuntimeConfig) error {
	if runtime == nil {
		return errors.New("runtime config is required")
	}
	if runtime.EnableGpu != nil || runtime.RemoteControlEnabled != nil {
		return errors.New("GPU enablement and remote control are install-only settings and require reinstalling the Agent")
	}
	if runtime.ReportInterval != nil {
		if err := runtime.ReportInterval.CheckValid(); err != nil {
			return err
		}
		seconds := runtime.ReportInterval.AsDuration().Seconds()
		profile.EnableInterval = seconds != 3
		profile.Interval = seconds
	}
	if runtime.TrafficResetDay != nil {
		if *runtime.TrafficResetDay > 31 {
			return errors.New("traffic reset day must be between 0 and 31")
		}
		profile.EnableMonthRotate = *runtime.TrafficResetDay > 0
		profile.MonthRotate = int(*runtime.TrafficResetDay)
	}
	profile.EnableIncludeNics = len(runtime.IncludeNics) > 0
	profile.IncludeNics = strings.Join(runtime.IncludeNics, ",")
	profile.EnableExcludeNics = len(runtime.ExcludeNics) > 0
	profile.ExcludeNics = strings.Join(runtime.ExcludeNics, ",")
	profile.EnableIncludeMountpoints = len(runtime.IncludeMountpoints) > 0
	profile.IncludeMountpoints = strings.Join(runtime.IncludeMountpoints, ";")
	if runtime.MemoryIncludeCache != nil {
		profile.MemoryIncludeCache = *runtime.MemoryIncludeCache
	}
	if runtime.DetailedGpu != nil {
		profile.DetailedGPU = *runtime.DetailedGpu
	}
	return nil
}

func deploymentToProto(agentID string, profile clients.DeploymentProfile) *deploymentv1.DeploymentProfile {
	return &deploymentv1.DeploymentProfile{
		AgentId: agentID,
		Install: &deploymentv1.InstallConfig{
			Platform:                platformToProto(profile.Platform),
			InstallDirectory:        profile.Dir,
			ServiceName:             profile.ServiceName,
			DisableAutoUpdate:       profile.DisableAutoUpdate,
			IgnoreUnsafeCertificate: profile.IgnoreUnsafeCert,
			EnableGithubProxy:       profile.EnableGHProxy,
			GithubProxy:             profile.GHProxy,
			RuntimeIdentity:         runtimeIdentityToProto(profile.RuntimeIdentity),
			EnableGpu:               profile.EnableGPU,
			RemoteControlEnabled:    profile.RemoteControlEnabled,
			DisableWebSsh:           profile.DisableWebSSH,
			GetIpAddressFromNic:     profile.GetIPAddrFromNIC,
			Rescue: &deploymentv1.RescueInstallConfig{
				Enabled:           profile.RescueEnabled,
				ConfigureFirewall: profile.RescueConfigureFirewall,
			},
		},
		Runtime: runtimeToProto(profile.RuntimeConfig()),
	}
}

func runtimeIdentityToProto(identity string) deploymentv1.AgentRuntimeIdentity {
	if identity == clients.AgentRuntimeIdentityCurrentUser {
		return deploymentv1.AgentRuntimeIdentity_AGENT_RUNTIME_IDENTITY_CURRENT_USER
	}
	return deploymentv1.AgentRuntimeIdentity_AGENT_RUNTIME_IDENTITY_ROOT_OR_ADMINISTRATOR
}

func runtimeIdentityFromProto(identity deploymentv1.AgentRuntimeIdentity) string {
	if identity == deploymentv1.AgentRuntimeIdentity_AGENT_RUNTIME_IDENTITY_CURRENT_USER {
		return clients.AgentRuntimeIdentityCurrentUser
	}
	return clients.AgentRuntimeIdentityPrivileged
}

func deliveryToProto(state clients.DeploymentDeliveryState) *deploymentv1.ConfigDelivery {
	result := &deploymentv1.ConfigDelivery{
		DesiredRevision: state.Revision,
		AppliedRevision: state.AppliedRevision,
		State:           deliveryState(state.Status),
		SavedAt:         timestamppb.New(state.SavedAt),
	}
	if state.Error != "" {
		result.Error = &commonv1.ErrorDetail{Code: "CONFIG_REJECTED", Message: state.Error}
	}
	if state.SentAt != nil {
		result.SentAt = timestamppb.New(*state.SentAt)
	}
	if state.FinishedAt != nil {
		result.FinishedAt = timestamppb.New(*state.FinishedAt)
	}
	return result
}

func splitList(value, separator string) []string {
	parts := strings.Split(value, separator)
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			result = append(result, part)
		}
	}
	return result
}

func platformToProto(platform string) deploymentv1.Platform {
	switch strings.ToLower(platform) {
	case "windows":
		return deploymentv1.Platform_PLATFORM_WINDOWS_AMD64
	case "macos", "darwin":
		return deploymentv1.Platform_PLATFORM_DARWIN_AMD64
	case "linux", "docker":
		return deploymentv1.Platform_PLATFORM_LINUX_AMD64
	default:
		return deploymentv1.Platform_PLATFORM_UNSPECIFIED
	}
}

func int64FromUint64(value uint64) int64 {
	if value > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(value)
}
