package connectapi

import (
	"strings"
	"testing"

	"github.com/komari-monitor/komari/database/clients"
	deploymentv1 "github.com/r11234567/komari-proto/gen/go/komari/deployment/v1"
)

func TestDeploymentCommandIncludesManagedAgentInstallOptions(t *testing.T) {
	command, err := deploymentCommand(clients.DeploymentProfile{
		Platform:             "linux",
		RuntimeIdentity:      clients.AgentRuntimeIdentityServiceAccount,
		RemoteControlEnabled: false,
		RescueEnabled:        true,
	}, "https://monitor.example.com", "agent-token")
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"r11234567/komari-agent/main/install.sh",
		"--install-runtime-identity", "service-account",
		"--disable-remote-control", "--install-rescue",
	} {
		if !strings.Contains(command, expected) {
			t.Fatalf("deployment command does not contain %q: %s", expected, command)
		}
	}
}

func TestDeploymentCommandRejectsRescueOnMacOS(t *testing.T) {
	_, err := deploymentCommand(clients.DeploymentProfile{
		Platform:             "macos",
		RuntimeIdentity:      clients.AgentRuntimeIdentityServiceAccount,
		RemoteControlEnabled: false,
		RescueEnabled:        true,
	}, "https://monitor.example.com", "agent-token")
	if err == nil {
		t.Fatal("deploymentCommand() accepted rescue mode on macOS")
	}
}

func TestDeploymentCommandDisablesRemoteControlForNonPrivilegedRuntime(t *testing.T) {
	command, err := deploymentCommand(clients.DeploymentProfile{
		Platform:             "linux",
		RuntimeIdentity:      clients.AgentRuntimeIdentityServiceAccount,
		RemoteControlEnabled: true,
	}, "https://monitor.example.com", "agent-token")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(command, "--disable-remote-control") {
		t.Fatalf("non-privileged install command enables remote control: %s", command)
	}
	if strings.Contains(command, "--disable-web-ssh") {
		t.Fatalf("install command retains deprecated WebSSH switch: %s", command)
	}
}

func TestNormalizeDeploymentEndpointUsesRequestOriginWhenSettingIsEmpty(t *testing.T) {
	endpoint, err := normalizeDeploymentEndpoint("", "https://monitor.example.com/")
	if err != nil {
		t.Fatal(err)
	}
	if endpoint != "https://monitor.example.com" {
		t.Fatalf("endpoint = %q", endpoint)
	}
}

func TestNormalizeDeploymentEndpointPrefersConfiguredDomain(t *testing.T) {
	endpoint, err := normalizeDeploymentEndpoint("https://agents.example.com/", "https://monitor.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if endpoint != "https://agents.example.com" {
		t.Fatalf("endpoint = %q", endpoint)
	}
}

func TestRuntimeIdentityUsesDedicatedServiceAccount(t *testing.T) {
	if got := runtimeIdentityToProto(clients.AgentRuntimeIdentityServiceAccount); got != deploymentv1.AgentRuntimeIdentity_AGENT_RUNTIME_IDENTITY_SERVICE_ACCOUNT {
		t.Fatalf("service account encoded as %s", got)
	}
	for _, wireValue := range []deploymentv1.AgentRuntimeIdentity{
		deploymentv1.AgentRuntimeIdentity_AGENT_RUNTIME_IDENTITY_SERVICE_ACCOUNT,
		deploymentv1.AgentRuntimeIdentity_AGENT_RUNTIME_IDENTITY_CURRENT_USER,
	} {
		if got := runtimeIdentityFromProto(wireValue); got != clients.AgentRuntimeIdentityServiceAccount {
			t.Fatalf("wire identity %s decoded as %q", wireValue, got)
		}
	}
}
