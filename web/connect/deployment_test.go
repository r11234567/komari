package connectapi

import (
	"strings"
	"testing"

	"github.com/komari-monitor/komari/database/clients"
)

func TestDeploymentCommandIncludesManagedAgentInstallOptions(t *testing.T) {
	command, err := deploymentCommand(clients.DeploymentProfile{
		Platform:                "linux",
		RuntimeIdentity:         clients.AgentRuntimeIdentityCurrentUser,
		RemoteControlEnabled:    false,
		RescueEnabled:           true,
		RescueConfigureFirewall: true,
	}, "https://monitor.example.com", "agent-token")
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"r11234567/komari-agent/main/install.sh",
		"--install-runtime-identity", "current-user",
		"--disable-remote-control", "--install-rescue", "--install-rescue-firewall",
	} {
		if !strings.Contains(command, expected) {
			t.Fatalf("deployment command does not contain %q: %s", expected, command)
		}
	}
}

func TestDeploymentCommandRejectsRescueOnMacOS(t *testing.T) {
	_, err := deploymentCommand(clients.DeploymentProfile{
		Platform:             "macos",
		RuntimeIdentity:      clients.AgentRuntimeIdentityCurrentUser,
		RemoteControlEnabled: false,
		RescueEnabled:        true,
	}, "https://monitor.example.com", "agent-token")
	if err == nil {
		t.Fatal("deploymentCommand() accepted rescue mode on macOS")
	}
}
