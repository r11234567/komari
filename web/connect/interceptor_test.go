package connectapi

import (
	"context"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/komari-monitor/komari/pkg/rpc"
)

func TestPolicyDeclaresEveryGeneratedProcedure(t *testing.T) {
	interceptor := newPolicyInterceptor()
	expected := []string{
		"/komari.browser.v1.BrowserService/GetPublicInfo",
		"/komari.browser.v1.BrowserService/ListAgents",
		"/komari.browser.v1.BrowserService/GetAgent",
		"/komari.browser.v1.BrowserService/WatchAgentStatus",
		"/komari.browser.v1.BrowserService/GetThemeContract",
		"/komari.config.v1.ConfigService/GetDesiredConfig",
		"/komari.config.v1.ConfigService/WatchDesiredConfig",
		"/komari.config.v1.ConfigService/AcknowledgeConfig",
		"/komari.config.v1.ConfigService/UpdateDesiredConfig",
		"/komari.deployment.v1.DeploymentService/GetDeployment",
		"/komari.deployment.v1.DeploymentService/SaveDeploymentProfile",
		"/komari.deployment.v1.DeploymentService/GenerateInstallCommand",
		"/komari.report.v1.AgentReportService/SubmitReport",
		"/komari.metrics.v1.MetricsService/SubmitMetrics",
		"/komari.metrics.v1.MetricsService/UploadMetrics",
		"/komari.metrics.v1.MetricsService/QueryMetrics",
		"/komari.metrics.v1.MetricsService/WatchMetrics",
		"/komari.exec.v1.ExecutionService/CreateExecution",
		"/komari.exec.v1.ExecutionService/WatchExecution",
		"/komari.exec.v1.ExecutionService/CancelExecution",
		"/komari.exec.v1.ExecutionService/GetExecution",
		"/komari.exec.v1.ExecutionService/LeaseExecution",
		"/komari.exec.v1.ExecutionService/ReportExecutionEvent",
		"/komari.webssh.v1.WebSSHService/OpenSession",
		"/komari.webssh.v1.WebSSHService/CloseSession",
		"/komari.agent.v1.AgentEventService/PublishEvent",
		"/komari.agent.v1.AgentEventService/SubscribeEvents",
		"/komari.agent.v1.AgentEventService/AcknowledgeEvent",
		"/komari.rescue.v1.RescueService/GetRescueStatus",
		"/komari.rescue.v1.RescueService/CreateRescueSession",
		"/komari.rescue.v1.RescueService/WatchRescueSession",
		"/komari.rescue.v1.RescueService/CancelRescueSession",
		"/komari.rescue.v1.RescueService/LeaseRescueSessions",
		"/komari.rescue.v1.RescueService/ReportRescueEvent",
		"/komari.rescue.v1.RescueService/ReportRescueStatus",
	}
	for _, procedure := range expected {
		if _, ok := interceptor.policies[procedure]; !ok {
			t.Errorf("procedure policy missing for %s", procedure)
		}
	}
	if len(interceptor.policies) != len(expected) {
		t.Fatalf("policy count = %d, expected %d", len(interceptor.policies), len(expected))
	}
}

func TestPolicyRejectsAnonymousAgentProcedure(t *testing.T) {
	ctx := rpc.NewContextWithMeta(context.Background(), &rpc.ContextMeta{Principal: rpc.NewAnonymousPrincipal()})
	_, cancel, err := newPolicyInterceptor().authorize(ctx, "/komari.report.v1.AgentReportService/SubmitReport")
	cancel()
	var connectErr *connect.Error
	if !errors.As(err, &connectErr) || connectErr.Code() != connect.CodePermissionDenied {
		t.Fatalf("authorize error = %v, want permission denied", err)
	}
}

func TestPolicyPreservesShorterClientDeadline(t *testing.T) {
	clientDeadline := time.Now().Add(2 * time.Second)
	ctx, clientCancel := context.WithDeadline(context.Background(), clientDeadline)
	defer clientCancel()
	ctx = rpc.NewContextWithMeta(ctx, &rpc.ContextMeta{Principal: rpc.NewAnonymousPrincipal()})
	bounded, cancel, err := newPolicyInterceptor().authorize(ctx, "/komari.browser.v1.BrowserService/GetPublicInfo")
	defer cancel()
	if err != nil {
		t.Fatalf("authorize error = %v", err)
	}
	deadline, ok := bounded.Deadline()
	if !ok || !deadline.Equal(clientDeadline) {
		t.Fatalf("deadline = %v, want %v", deadline, clientDeadline)
	}
}

func TestRequireAgentRejectsSpoofedAgentID(t *testing.T) {
	meta := &rpc.ContextMeta{Principal: rpc.NewAgentPrincipal("node-a")}
	if _, err := requireAgent(meta, "node-b"); err == nil {
		t.Fatal("spoofed agent ID was accepted")
	}
	if got, err := requireAgent(meta, "node-a"); err != nil || got != "node-a" {
		t.Fatalf("authenticated agent rejected: id=%q err=%v", got, err)
	}
}
