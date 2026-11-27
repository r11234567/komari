package connectapi

import (
	"net/http"
	"strings"

	"connectrpc.com/connect"
	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/internal/diagnostics"
	"github.com/komari-monitor/komari/internal/passkeycredential"
	"github.com/komari-monitor/komari/pkg/requestmeta"
	"github.com/komari-monitor/komari/pkg/rpc"
	"github.com/komari-monitor/komari/web/api"
	"github.com/komari-monitor/komari/web/security"
	"github.com/komari-monitor/komari/web/workspace"
	adminv1connect "github.com/r11234567/komari-proto/gen/go/komari/admin/v1/adminv1connect"
	agentv1connect "github.com/r11234567/komari-proto/gen/go/komari/agent/v1/agentv1connect"
	browserv1connect "github.com/r11234567/komari-proto/gen/go/komari/browser/v1/browserv1connect"
	configv1connect "github.com/r11234567/komari-proto/gen/go/komari/config/v1/configv1connect"
	credentialv1connect "github.com/r11234567/komari-proto/gen/go/komari/credential/v1/credentialv1connect"
	deploymentv1connect "github.com/r11234567/komari-proto/gen/go/komari/deployment/v1/deploymentv1connect"
	enrollmentv1connect "github.com/r11234567/komari-proto/gen/go/komari/enrollment/v1/enrollmentv1connect"
	execv1connect "github.com/r11234567/komari-proto/gen/go/komari/exec/v1/execv1connect"
	filev1connect "github.com/r11234567/komari-proto/gen/go/komari/file/v1/filev1connect"
	hostv1connect "github.com/r11234567/komari-proto/gen/go/komari/host/v1/hostv1connect"
	metricsv1connect "github.com/r11234567/komari-proto/gen/go/komari/metrics/v1/metricsv1connect"
	networkv1connect "github.com/r11234567/komari-proto/gen/go/komari/network/v1/networkv1connect"
	pluginv1connect "github.com/r11234567/komari-proto/gen/go/komari/plugin/v1/pluginv1connect"
	reportv1connect "github.com/r11234567/komari-proto/gen/go/komari/report/v1/reportv1connect"
	rescuev1connect "github.com/r11234567/komari-proto/gen/go/komari/rescue/v1/rescuev1connect"
	websshv1connect "github.com/r11234567/komari-proto/gen/go/komari/webssh/v1/websshv1connect"
	workspacev1connect "github.com/r11234567/komari-proto/gen/go/komari/workspace/v1/workspacev1connect"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

const maxConnectRequestBytes = 4 << 20

// Restricted startup listeners expose only the existing native password
// ceremony, including its configured second factor, never operational RPCs.
func RegisterRestrictedPasswordLogin(r *gin.Engine) {
	path, handler := credentialv1connect.NewPasswordLoginServiceHandler(&passwordLoginService{}, connect.WithInterceptors(diagnostics.OutcomeInterceptor{}, newPolicyInterceptor()), connect.WithReadMaxBytes(128<<10))
	r.Any(strings.TrimSuffix(path, "/")+"/*method", bridge(handler, registerDiagnosticProcedures(path)))
}

// Register mounts each generated Connect service at its canonical procedure path.
func Register(r *gin.Engine) {
	policy := newPolicyInterceptor()
	opts := []connect.HandlerOption{
		connect.WithInterceptors(diagnostics.OutcomeInterceptor{}, policy),
		connect.WithReadMaxBytes(maxConnectRequestBytes),
	}
	r.Any(sealedRecordProcedure, bridge(sealedRecordHandler(
		connect.WithInterceptors(diagnostics.OutcomeInterceptor{}, policy),
		connect.WithReadMaxBytes(384<<10),
	), nil))
	r.Any(listSealedProcedure, bridge(connect.NewUnaryHandler(listSealedProcedure, listSealedRecords,
		connect.WithInterceptors(policy), connect.WithReadMaxBytes(1024)), nil))
	r.Any(downloadSealedProcedure, bridge(connect.NewUnaryHandler(downloadSealedProcedure, downloadSealedRecord,
		connect.WithInterceptors(policy), connect.WithReadMaxBytes(1024)), nil))
	handlers := []struct {
		path    string
		handler http.Handler
	}{
		newHandler(credentialv1connect.NewPasswordLoginServiceHandler(&passwordLoginService{}, connect.WithInterceptors(diagnostics.OutcomeInterceptor{}, policy), connect.WithReadMaxBytes(128<<10))),
		newHandler(credentialv1connect.NewPasskeyLoginServiceHandler(passkeyService{}, connect.WithInterceptors(diagnostics.OutcomeInterceptor{}, policy), connect.WithReadMaxBytes(128<<10))),
		newHandler(credentialv1connect.NewPasskeyManagementServiceHandler(passkeyService{}, connect.WithInterceptors(diagnostics.OutcomeInterceptor{}, policy), connect.WithReadMaxBytes(128<<10))),
		newHandler(credentialv1connect.NewTrustedCredentialServiceHandler(&credentialService{}, connect.WithInterceptors(diagnostics.OutcomeInterceptor{}, policy), connect.WithReadMaxBytes(128<<10))),
		newHandler(browserv1connect.NewBrowserServiceHandler(&browserService{}, opts...)),
		newHandler(adminv1connect.NewDashboardServiceHandler(&dashboardService{}, opts...)),
		newHandler(adminv1connect.NewApiTokenServiceHandler(&apiTokenService{}, opts...)),
		newHandler(adminv1connect.NewMaintenanceServiceHandler(&maintenanceService{}, opts...)),
		newHandler(adminv1connect.NewTerminalSettingsServiceHandler(&terminalSettingsService{}, opts...)),
		newHandler(filev1connect.NewFileServiceHandler(newFileService(), opts...)),
		newHandler(adminv1connect.NewPingTaskServiceHandler(&pingTaskService{}, opts...)),
		newHandler(hostv1connect.NewHostServiceHandler(&hostService{}, opts...)),
		newHandler(configv1connect.NewConfigServiceHandler(&configService{}, opts...)),
		newHandler(configv1connect.NewPrivilegedDeliveryServiceHandler(&privilegedDeliveryService{}, opts...)),
		newHandler(enrollmentv1connect.NewEnrollmentServiceHandler(&enrollmentService{}, opts...)),
		newHandler(enrollmentv1connect.NewEnrollmentAdminServiceHandler(&enrollmentAdminService{}, opts...)),
		newHandler(deploymentv1connect.NewDeploymentServiceHandler(&deploymentService{}, opts...)),
		newHandler(reportv1connect.NewAgentReportServiceHandler(&reportService{}, opts...)),
		newHandler(metricsv1connect.NewMetricsServiceHandler(&metricsService{}, opts...)),
		newHandler(pluginv1connect.NewPluginServiceHandler(&pluginService{}, opts...)),
		newHandler(networkv1connect.NewNetworkProbeServiceHandler(&networkProbeService{}, opts...)),
		newHandler(rescuev1connect.NewRescueServiceHandler(&rescueService{}, opts...)),
		newHandler(execv1connect.NewExecutionServiceHandler(&executionService{}, opts...)),
		newHandler(workspacev1connect.NewWorkspaceServiceHandler(&workspaceService{}, opts...)),
		newHandler(websshv1connect.NewWebSSHServiceHandler(&webSSHService{}, opts...)),
		newHandler(agentv1connect.NewAgentEventServiceHandler(&agentEventService{}, opts...)),
	}
	for _, item := range handlers {
		pattern := strings.TrimSuffix(item.path, "/") + "/*method"
		r.Any(pattern, bridge(item.handler, registerDiagnosticProcedures(item.path)))
	}
}

// RegisterPublic mounts only the read-only public Connect procedures on the
// dedicated Public Listener and forces every call onto an anonymous PublicViewer
// context, leaving all management and Agent control procedures unmounted (404).
func RegisterPublic(r *gin.Engine) {
	policy := newPolicyInterceptor()
	opts := []connect.HandlerOption{
		connect.WithInterceptors(diagnostics.OutcomeInterceptor{}, policy),
		connect.WithReadMaxBytes(maxConnectRequestBytes),
	}
	browserPath, browserHandler := browserv1connect.NewBrowserServiceHandler(&browserService{}, opts...)
	registerDiagnosticProcedures(browserPath)
	for _, method := range []string{
		"GetPublicInfo",
		"ListAgents",
		"GetAgent",
		"GetSession",
		"GetThemeContract",
		"WatchAgentStatus",
	} {
		r.Any(browserPath+method, bridgePublic(browserHandler))
	}

	metricsPath, metricsHandler := metricsv1connect.NewMetricsServiceHandler(&metricsService{}, opts...)
	registerDiagnosticProcedures(metricsPath)
	for _, method := range []string{
		"QueryMetrics",
		"ListMetricDefinitions",
		"ListPingTasks",
		"GetPingStats",
	} {
		r.Any(metricsPath+method, bridgePublic(metricsHandler))
	}
}

func bridgePublic(handler http.Handler) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !allowConnectMethod(c, true) {
			return
		}
		anon := rpc.NewAnonymousPrincipal()
		security.SetPrincipalHeader(c, anon)
		meta := &rpc.ContextMeta{
			Principal:      anon,
			Permission:     anon.PrimaryRole(),
			RemoteIP:       security.AuthSourceIP(c),
			UserAgent:      c.Request.UserAgent(),
			TempShareValid: api.HasTempAccess(c),
		}
		ctx := rpc.NewContextWithMeta(c.Request.Context(), meta)
		c.Request = c.Request.WithContext(ctx)
		handler.ServeHTTP(c.Writer, c.Request)
	}
}

func registerDiagnosticProcedures(path string) map[string]bool {
	name := protoreflect.FullName(strings.Trim(path, "/"))
	descriptor, err := protoregistry.GlobalFiles.FindDescriptorByName(name)
	if err != nil {
		return nil
	}
	service, ok := descriptor.(protoreflect.ServiceDescriptor)
	if !ok {
		return nil
	}
	streams := make(map[string]bool)
	for i := 0; i < service.Methods().Len(); i++ {
		method := service.Methods().Get(i)
		procedure := path + string(method.Name())
		streaming := method.IsStreamingClient() || method.IsStreamingServer()
		diagnostics.Default.RegisterProcedure(procedure, streaming)
		if streaming {
			streams[procedure] = true
		}
	}
	return streams
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

func bridge(handler http.Handler, streams map[string]bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !allowConnectMethod(c, false) {
			return
		}
		req, err := requestmeta.EnsureBound(c.Request)
		if err != nil {
			security.AbortRequest(c, http.StatusBadRequest, "invalid request origin")
			return
		}
		origin, err := requestmeta.Origin(req)
		if err != nil {
			security.AbortRequest(c, http.StatusBadRequest, "invalid request origin")
			return
		}
		c.Request = req
		switch req.URL.Path {
		case enrollmentv1connect.EnrollmentAdminServiceGetPendingEnrollmentProcedure,
			enrollmentv1connect.EnrollmentAdminServiceApproveEnrollmentProcedure,
			enrollmentv1connect.EnrollmentAdminServiceDenyEnrollmentProcedure:
			if !security.SameOriginBindRequest(req) {
				security.AbortRequest(c, http.StatusForbidden, "Bind authorization requires a same-origin request")
				return
			}
		}
		principal := api.GetPrincipal(c)
		if principal == nil {
			principal = api.IdentifyPrincipal(c)
		}
		if err := verifyDPoP(c, principal); err != nil {
			security.SetPrincipalHeader(c, rpc.NewAnonymousPrincipal())
			security.AbortRequest(c, http.StatusUnauthorized, err.Error())
			return
		}
		meta := &rpc.ContextMeta{
			Principal:      principal,
			Permission:     principal.PrimaryRole(),
			UserUUID:       principal.UserUUID,
			ClientUUID:     principal.ClientUUID,
			RemoteIP:       security.AuthSourceIP(c),
			UserAgent:      c.Request.UserAgent(),
			TempShareValid: api.HasTempAccess(c),
		}
		// The session list marks the caller's own credential, so the token has to
		// reach Connect handlers the same way it reaches the legacy transport.
		if principal.Type == rpc.PrincipalUser {
			if session, err := c.Cookie(security.SessionCookieName); err == nil {
				meta.SessionToken = session
			}
		}
		scheme, host, _ := strings.Cut(origin, "://")
		secure := scheme == "https"
		ctx := passkeycredential.WithRequestOrigin(rpc.NewContextWithMeta(req.Context(), meta), host, secure)
		c.Request = req.WithContext(workspace.WithSecure(workspace.WithToken(ctx, c.GetHeader(workspace.Header)), secure))
		if streams[req.URL.Path] && principal.Type == rpc.PrincipalAgent {
			// Rescue watches already recheck credentials once per second and
			// use a final signed frame to withdraw a temporary SSH rule.
			rescueWatch := req.URL.Path == configv1connect.PrivilegedDeliveryServiceWatchPrivilegedDeliveryProcedure &&
				c.GetHeader(rescueWatchHeader) == "v1"
			if !rescueWatch {
				serveAgentStream(c, principal, func() { handler.ServeHTTP(c.Writer, c.Request) })
				return
			}
		}
		handler.ServeHTTP(c.Writer, c.Request)
	}
}

func allowConnectMethod(c *gin.Context, publicListener bool) bool {
	// Private responses can depend on a console session, Agent identity or
	// temporary share cookie. Proxy and browser caches must not store them.
	c.Header("Cache-Control", "no-store")
	if c.Request.Method != http.MethodGet {
		return true
	}
	switch c.Request.URL.Path {
	case browserv1connect.BrowserServiceGetPublicInfoProcedure, browserv1connect.BrowserServiceGetThemeContractProcedure,
		credentialv1connect.PasskeyLoginServiceGetPasskeyStatusProcedure,
		metricsv1connect.MetricsServiceListMetricDefinitionsProcedure, metricsv1connect.MetricsServiceListPingTasksProcedure:
		return true
	case browserv1connect.BrowserServiceListAgentsProcedure, browserv1connect.BrowserServiceGetAgentProcedure:
		if publicListener {
			return true
		}
	}
	// Historical protocol descriptors retain their idempotency annotations.
	// Enforce the transport method here without changing those contracts.
	c.Header("Allow", http.MethodPost)
	c.AbortWithStatus(http.StatusMethodNotAllowed)
	return false
}
