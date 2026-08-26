package connectapi

import (
	"context"
	"errors"
	"time"

	"connectrpc.com/connect"
	"github.com/komari-monitor/komari/pkg/rpc"
)

type procedurePolicy struct {
	role        string
	maxDuration time.Duration
}

type policyInterceptor struct {
	policies map[string]procedurePolicy
}

func newPolicyInterceptor() *policyInterceptor {
	const (
		dashboard   = "/komari.admin.v1.DashboardService/"
		maintenance = "/komari.admin.v1.MaintenanceService/"
		pingTask    = "/komari.admin.v1.PingTaskService/"
		browser     = "/komari.browser.v1.BrowserService/"
		config      = "/komari.config.v1.ConfigService/"
		deploy      = "/komari.deployment.v1.DeploymentService/"
		report      = "/komari.report.v1.AgentReportService/"
		metrics     = "/komari.metrics.v1.MetricsService/"
		plugin      = "/komari.plugin.v1.PluginService/"
		network     = "/komari.network.v1.NetworkProbeService/"
		exec        = "/komari.exec.v1.ExecutionService/"
		webssh      = "/komari.webssh.v1.WebSSHService/"
		events      = "/komari.agent.v1.AgentEventService/"
		rescue      = "/komari.rescue.v1.RescueService/"
	)
	policies := map[string]procedurePolicy{}
	add := func(prefix, role string, timeout time.Duration, methods ...string) {
		for _, method := range methods {
			policies[prefix+method] = procedurePolicy{role: role, maxDuration: timeout}
		}
	}
	add(dashboard, rpc.RoleAdmin, 30*time.Second, "GetDashboardSummary", "GetDashboardCharts", "ListDashboardAlertItems")
	add(maintenance, rpc.RoleAdmin, 30*time.Second, "ListSessions", "DeleteSession", "DeleteAllSessions", "ListAuditLogs",
		"ListClipboardEntries", "GetClipboardEntry", "CreateClipboardEntry", "UpdateClipboardEntry", "DeleteClipboardEntries",
		"ClearRecords", "GetDatabaseStatus")
	// Reclaiming storage rewrites whole tables and routinely outlives a normal
	// admin request on large SQLite deployments.
	add(maintenance, rpc.RoleAdmin, 30*time.Minute, "VacuumDatabase")
	add(pingTask, rpc.RoleAdmin, 30*time.Second, "ListPingTasks", "CreatePingTask", "UpdatePingTasks", "DeletePingTasks", "ReorderPingTasks")
	add(browser, rpc.RoleGuest, 30*time.Second, "GetPublicInfo", "ListAgents", "GetAgent", "GetSession", "GetThemeContract")
	add(browser, rpc.RoleAdmin, 30*time.Second, "GetTrafficTrend")
	add(browser, rpc.RoleGuest, 30*time.Minute, "WatchAgentStatus")
	add(config, rpc.RoleClient, 30*time.Second, "GetDesiredConfig", "AcknowledgeConfig")
	add(config, rpc.RoleClient, 30*time.Minute, "WatchDesiredConfig")
	add(config, rpc.RoleAdmin, 30*time.Second, "UpdateDesiredConfig")
	add(deploy, rpc.RoleAdmin, 30*time.Second, "GetDeployment", "SaveDeploymentProfile", "GenerateInstallCommand")
	add(report, rpc.RoleClient, 15*time.Second, "SubmitReport")
	add(metrics, rpc.RoleClient, 30*time.Second, "SubmitMetrics")
	add(metrics, rpc.RoleClient, 5*time.Minute, "UploadMetrics")
	add(metrics, rpc.RoleClient, 30*time.Minute, "StreamMetrics")
	add(metrics, rpc.RoleGuest, 30*time.Second, "QueryMetrics", "ListMetricDefinitions", "ListPingTasks", "GetPingStats")
	add(metrics, rpc.RoleAdmin, 30*time.Minute, "WatchMetrics")
	add(metrics, rpc.RoleAdmin, 30*time.Second, "UpdateMetricDefinition", "GetDownsamplingPolicy", "SetDownsamplingPolicy", "GetMetricMigrationStatus", "StartMetricMigration", "CancelMetricMigration")
	add(plugin, rpc.RoleAdmin, 30*time.Second, "ListPlugins", "SetPluginEnabled", "GetPluginLogs", "DeletePlugin", "GetPluginConfiguration", "SetPluginConfiguration")
	add(network, rpc.RoleClient, 30*time.Second, "LeasePingProbe", "SubmitPingProbeResult", "LeaseReturnRouteProbe", "SubmitReturnRouteProbeResult")
	add(exec, rpc.RoleAdmin, 30*time.Second, "CreateExecution", "CancelExecution", "GetExecution")
	add(exec, rpc.RoleAdmin, 30*time.Minute, "WatchExecution")
	add(exec, rpc.RoleClient, 30*time.Minute, "LeaseExecution")
	add(exec, rpc.RoleClient, 30*time.Second, "ReportExecutionEvent")
	add(webssh, rpc.RoleAdmin, 2*time.Hour, "OpenSession")
	add(webssh, rpc.RoleAdmin, 30*time.Minute, "WatchSession")
	add(webssh, rpc.RoleAdmin, 15*time.Second, "CreateSession", "SendSessionCommand", "AcknowledgeSessionEvents", "CloseSession")
	add(webssh, rpc.RoleClient, 30*time.Minute, "LeaseSessions")
	add(webssh, rpc.RoleClient, 2*time.Hour, "AttachSession")
	add(events, rpc.RoleClient, 30*time.Second, "PublishEvent", "AcknowledgeEvent")
	add(events, rpc.RoleClient, 30*time.Minute, "SubscribeEvents")
	add(rescue, rpc.RoleAdmin, 30*time.Second, "GetRescueStatus", "CreateRescueSession", "CancelRescueSession")
	add(rescue, rpc.RoleAdmin, 30*time.Minute, "WatchRescueSession")
	add(rescue, rpc.RoleClient, 30*time.Minute, "LeaseRescueSessions")
	add(rescue, rpc.RoleClient, 30*time.Second, "ReportRescueEvent", "ReportRescueStatus")
	return &policyInterceptor{policies: policies}
}

func (i *policyInterceptor) authorize(ctx context.Context, procedure string) (context.Context, context.CancelFunc, error) {
	policy, ok := i.policies[procedure]
	if !ok {
		return ctx, func() {}, connect.NewError(connect.CodePermissionDenied, errors.New("procedure policy is not declared"))
	}
	meta := rpc.MetaFromContext(ctx)
	if meta == nil || meta.Principal == nil {
		return ctx, func() {}, connect.NewError(connect.CodeUnauthenticated, errors.New("authentication context is missing"))
	}
	if policy.role != rpc.RoleGuest && !meta.Principal.HasRole(policy.role) {
		return ctx, func() {}, connect.NewError(connect.CodePermissionDenied, errors.New("permission denied"))
	}
	if policy.maxDuration <= 0 {
		return ctx, func() {}, nil
	}
	deadline := time.Now().Add(policy.maxDuration)
	if existing, ok := ctx.Deadline(); ok && existing.Before(deadline) {
		return ctx, func() {}, nil
	}
	bounded, cancel := context.WithDeadline(ctx, deadline)
	return bounded, cancel, nil
}

func (i *policyInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		bounded, cancel, err := i.authorize(ctx, req.Spec().Procedure)
		if err != nil {
			return nil, err
		}
		defer cancel()
		return next(bounded, req)
	}
}

func (i *policyInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (i *policyInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		bounded, cancel, err := i.authorize(ctx, conn.Spec().Procedure)
		if err != nil {
			return err
		}
		defer cancel()
		return next(bounded, conn)
	}
}
