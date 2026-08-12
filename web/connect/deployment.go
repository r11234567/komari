package connectapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"connectrpc.com/connect"
	"github.com/komari-monitor/komari/database/clients"
	"github.com/komari-monitor/komari/pkg/config"
	agent_runtime "github.com/komari-monitor/komari/web/agent"
	deploymentapp "github.com/komari-monitor/komari/web/deployment"
	"github.com/komari-monitor/komari/web/rescueapp"
	deploymentv1 "github.com/r11234567/komari-proto/gen/go/komari/deployment/v1"
	deploymentv1connect "github.com/r11234567/komari-proto/gen/go/komari/deployment/v1/deploymentv1connect"
)

type deploymentService struct {
	deploymentv1connect.UnimplementedDeploymentServiceHandler
}

func (s *deploymentService) GenerateInstallCommand(_ context.Context, req *connect.Request[deploymentv1.GenerateInstallCommandRequest]) (*connect.Response[deploymentv1.GenerateInstallCommandResponse], error) {
	if strings.TrimSpace(req.Msg.AgentId) == "" {
		return nil, connectError(connect.CodeInvalidArgument, errors.New("agent ID is required"))
	}
	profile, saved, err := clients.GetDeploymentProfile(req.Msg.AgentId)
	if err != nil {
		return nil, connectError(connect.CodeNotFound, errors.New("agent not found"))
	}
	if !saved {
		return nil, connectError(connect.CodeFailedPrecondition, errors.New("save the deployment profile before generating an install command"))
	}
	agent, err := clients.GetClientByUUID(req.Msg.AgentId)
	if err != nil {
		return nil, connectError(connect.CodeNotFound, errors.New("agent not found"))
	}
	endpoint, err := deploymentEndpoint(req)
	if err != nil {
		return nil, connectError(connect.CodeFailedPrecondition, err)
	}
	platform := platformFromProto(req.Msg.Platform)
	profile.Platform = platform
	command, err := deploymentCommand(profile, endpoint, agent.Token)
	if err != nil {
		return nil, connectError(connect.CodeInvalidArgument, err)
	}
	sum := sha256.Sum256([]byte(command))
	return connect.NewResponse(&deploymentv1.GenerateInstallCommandResponse{
		Command: command,
		Sha256:  hex.EncodeToString(sum[:]),
	}), nil
}

func deploymentEndpoint(req *connect.Request[deploymentv1.GenerateInstallCommandRequest]) (string, error) {
	value, err := config.GetAs[string](config.ScriptDomainKey, "")
	if err != nil {
		return "", fmt.Errorf("read script domain: %w", err)
	}
	return normalizeDeploymentEndpoint(value, req.Header().Get("Origin"))
}

func normalizeDeploymentEndpoint(configured, requestOrigin string) (string, error) {
	value := strings.TrimRight(strings.TrimSpace(configured), "/")
	if value == "" {
		value = strings.TrimRight(strings.TrimSpace(requestOrigin), "/")
	}
	parsed, parseErr := url.Parse(value)
	if parseErr != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", errors.New("configure an HTTP(S) script domain before generating install commands")
	}
	return value, nil
}

func deploymentCommand(profile clients.DeploymentProfile, endpoint, token string) (string, error) {
	arguments := []string{"--install-runtime-identity", profile.RuntimeIdentity, "--endpoint", endpoint, "--token", token}
	if profile.EnableCustomDir && profile.Dir != "" {
		arguments = append(arguments, "--install-dir", profile.Dir)
	}
	if profile.EnableCustomServiceName && profile.ServiceName != "" {
		arguments = append(arguments, "--install-service-name", profile.ServiceName)
	}
	if profile.EnableGHProxy && profile.GHProxy != "" {
		arguments = append(arguments, "--install-ghproxy", profile.GHProxy)
	}
	if profile.DisableAutoUpdate {
		arguments = append(arguments, "--disable-auto-update")
	}
	if profile.IgnoreUnsafeCert {
		arguments = append(arguments, "--ignore-unsafe-cert")
	}
	if profile.EnableGPU {
		arguments = append(arguments, "--enable-gpu")
	}
	if profile.DetailedGPU {
		arguments = append(arguments, "--detailed-gpu")
	}
	if profile.GetIPAddrFromNIC {
		arguments = append(arguments, "--get-ip-addr-from-nic")
	}
	if !profile.RemoteControlEnabled {
		arguments = append(arguments, "--disable-remote-control")
	}
	if profile.DisableWebSSH {
		arguments = append(arguments, "--disable-web-ssh")
	}
	if profile.RescueEnabled {
		arguments = append(arguments, "--install-rescue")
		if profile.RescueConfigureFirewall {
			arguments = append(arguments, "--install-rescue-firewall")
		}
	}
	script := "https://raw.githubusercontent.com/r11234567/komari-agent/main/install.sh"
	switch profile.Platform {
	case "windows":
		script = "https://raw.githubusercontent.com/r11234567/komari-agent/main/install.ps1"
		return "powershell.exe -NoProfile -ExecutionPolicy Bypass -Command \"iwr " + powerShellQuote(script) + " -UseBasicParsing -OutFile 'install.ps1'; & '.\\install.ps1' " + powerShellArguments(arguments) + "\"", nil
	case "linux":
		return "curl -fsSL " + shellQuote(script) + " | sudo bash -s -- " + shellArguments(arguments), nil
	case "macos":
		if profile.RescueEnabled {
			return "", errors.New("the privileged rescue helper is currently supported only on Linux and Windows")
		}
		return "curl -fsSL " + shellQuote(script) + " | bash -s -- " + shellArguments(arguments), nil
	default:
		return "", errors.New("install command is supported only for Linux, Windows, and macOS")
	}
}

func shellArguments(values []string) string {
	quoted := make([]string, len(values))
	for index, value := range values {
		quoted[index] = shellQuote(value)
	}
	return strings.Join(quoted, " ")
}

func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'" }

func powerShellArguments(values []string) string {
	quoted := make([]string, len(values))
	for index, value := range values {
		quoted[index] = powerShellQuote(value)
	}
	return strings.Join(quoted, " ")
}

func powerShellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "''") + "'" }

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
	return connect.NewResponse(&deploymentv1.GetDeploymentResponse{
		Profile:      deploymentToProto(req.Msg.AgentId, profile),
		Delivery:     deliveryToProto(state),
		RescueHelper: rescueStatus,
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
		profile.EnableGPU = install.EnableGpu
		profile.RemoteControlEnabled = install.RemoteControlEnabled
		profile.DisableWebSSH = install.DisableWebSsh
		profile.GetIPAddrFromNIC = install.GetIpAddressFromNic
		profile.EnableGHProxy = install.EnableGithubProxy
		profile.GHProxy = install.GithubProxy
		profile.RuntimeIdentity = runtimeIdentityFromProto(install.RuntimeIdentity)
		if install.Rescue != nil {
			profile.RescueEnabled = install.Rescue.Enabled
			profile.RescueConfigureFirewall = install.Rescue.ConfigureFirewall
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
