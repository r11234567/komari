package connectapi

import (
	agentv1connect "github.com/r11234567/komari-proto/gen/go/komari/agent/v1/agentv1connect"
	execv1connect "github.com/r11234567/komari-proto/gen/go/komari/exec/v1/execv1connect"
	metricsv1connect "github.com/r11234567/komari-proto/gen/go/komari/metrics/v1/metricsv1connect"
	websshv1connect "github.com/r11234567/komari-proto/gen/go/komari/webssh/v1/websshv1connect"
)

type unimplementedMetricsService struct {
	metricsv1connect.UnimplementedMetricsServiceHandler
}

type unimplementedExecutionService struct {
	execv1connect.UnimplementedExecutionServiceHandler
}

type unimplementedWebSSHService struct {
	websshv1connect.UnimplementedWebSSHServiceHandler
}

type unimplementedAgentEventService struct {
	agentv1connect.UnimplementedAgentEventServiceHandler
}
