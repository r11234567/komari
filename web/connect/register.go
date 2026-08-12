package connectapi

import (
	"net/http"
	"strings"

	"connectrpc.com/connect"
	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/pkg/rpc"
	"github.com/komari-monitor/komari/web/api"
	agentv1connect "github.com/r11234567/komari-proto/gen/go/komari/agent/v1/agentv1connect"
	browserv1connect "github.com/r11234567/komari-proto/gen/go/komari/browser/v1/browserv1connect"
	configv1connect "github.com/r11234567/komari-proto/gen/go/komari/config/v1/configv1connect"
	deploymentv1connect "github.com/r11234567/komari-proto/gen/go/komari/deployment/v1/deploymentv1connect"
	execv1connect "github.com/r11234567/komari-proto/gen/go/komari/exec/v1/execv1connect"
	metricsv1connect "github.com/r11234567/komari-proto/gen/go/komari/metrics/v1/metricsv1connect"
	reportv1connect "github.com/r11234567/komari-proto/gen/go/komari/report/v1/reportv1connect"
	websshv1connect "github.com/r11234567/komari-proto/gen/go/komari/webssh/v1/websshv1connect"
)

const maxConnectRequestBytes = 4 << 20

// Register mounts each generated Connect service at its canonical procedure path.
func Register(r *gin.Engine) {
	policy := newPolicyInterceptor()
	opts := []connect.HandlerOption{
		connect.WithInterceptors(policy),
		connect.WithReadMaxBytes(maxConnectRequestBytes),
	}
	handlers := []struct {
		path    string
		handler http.Handler
	}{
		newHandler(browserv1connect.NewBrowserServiceHandler(&browserService{}, opts...)),
		newHandler(configv1connect.NewConfigServiceHandler(&configService{}, opts...)),
		newHandler(deploymentv1connect.NewDeploymentServiceHandler(&deploymentService{}, opts...)),
		newHandler(reportv1connect.NewAgentReportServiceHandler(&reportService{}, opts...)),
		newHandler(metricsv1connect.NewMetricsServiceHandler(&metricsService{}, opts...)),
		newHandler(execv1connect.NewExecutionServiceHandler(&unimplementedExecutionService{}, opts...)),
		newHandler(websshv1connect.NewWebSSHServiceHandler(&unimplementedWebSSHService{}, opts...)),
		newHandler(agentv1connect.NewAgentEventServiceHandler(&unimplementedAgentEventService{}, opts...)),
	}
	for _, item := range handlers {
		pattern := strings.TrimSuffix(item.path, "/") + "/*method"
		r.Any(pattern, bridge(item.handler))
	}
}

func newHandler(path string, handler http.Handler) struct {
	path    string
	handler http.Handler
} {
	return struct {
		path    string
		handler http.Handler
	}{path: path, handler: handler}
}

func bridge(handler http.Handler) gin.HandlerFunc {
	return func(c *gin.Context) {
		principal := api.GetPrincipal(c)
		if principal == nil {
			principal = api.IdentifyPrincipal(c)
		}
		meta := &rpc.ContextMeta{
			Principal:      principal,
			Permission:     principal.PrimaryRole(),
			UserUUID:       principal.UserUUID,
			ClientUUID:     principal.ClientUUID,
			RemoteIP:       c.ClientIP(),
			UserAgent:      c.Request.UserAgent(),
			TempShareValid: api.HasTempAccess(c),
		}
		c.Request = c.Request.WithContext(rpc.NewContextWithMeta(c.Request.Context(), meta))
		handler.ServeHTTP(c.Writer, c.Request)
	}
}
