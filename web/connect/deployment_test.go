package connectapi

import (
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"

	"github.com/komari-monitor/komari/database/clients"
	"github.com/komari-monitor/komari/pkg/requestmeta"
	deploymentv1 "github.com/r11234567/komari-proto/gen/go/komari/deployment/v1"
)

func TestGenerateInstallCommandIsRetired(t *testing.T) {
	service := &deploymentService{}
	response, err := service.GenerateInstallCommand(t.Context(), connect.NewRequest(&deploymentv1.GenerateInstallCommandRequest{AgentId: "existing-agent"}))
	if response != nil || connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Fatalf("retired install RPC = (%v, %v)", response, err)
	}
}

func TestDeploymentEndpointUsesDestinationNotCORSOrigin(t *testing.T) {
	policy, err := requestmeta.NewPolicy("none", "")
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "https://monitor.example.com/install", nil)
	r.Header.Set("Origin", "https://different-console.example.com")
	endpoint, err := requestmeta.PublicURL(policy.BindRequest(r).Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	if endpoint != "https://monitor.example.com" {
		t.Fatalf("endpoint = %q, want destination origin", endpoint)
	}
}

func TestNormalizeDeploymentEndpointPrefersConfiguredDomain(t *testing.T) {
	policy, err := requestmeta.NewPolicy("none", "")
	if err != nil {
		t.Fatal(err)
	}
	r := policy.BindRequest(httptest.NewRequest("POST", "https://monitor.example.com/install", nil))
	endpoint, err := requestmeta.PublicURL(r.Context(), "https://agents.example.com/")
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
