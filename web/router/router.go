package router

import (
	"os"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/web/api"
	"github.com/komari-monitor/komari/web/api/admin"
	"github.com/komari-monitor/komari/web/api/client"
	public_api "github.com/komari-monitor/komari/web/api/public"
	"github.com/komari-monitor/komari/web/api/remote"
	"github.com/komari-monitor/komari/web/api/terminal"
	connectapi "github.com/komari-monitor/komari/web/connect"
	installweb "github.com/komari-monitor/komari/web/install"
	"github.com/komari-monitor/komari/web/public"
	jsonRpc "github.com/komari-monitor/komari/web/rpc/jsonrpc"
)

// legacyRemoteCompatEnabled reports whether the pre-Connect terminal and remote
// control endpoints are mounted.
//
// Connect WebSSH (komari.webssh.v1) is the production transport for both the
// browser and the Agent. The WebSocket pair below only ever reaches an Agent
// that never established a Connect lease, so leaving it mounted would keep a
// second, weaker authorization path open for no production benefit. Operators
// who still run pre-Connect Agents and need a terminal there can opt back in.
func legacyRemoteCompatEnabled() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("KOMARI_LEGACY_REMOTE_COMPAT")), "true")
}

// Register binds all HTTP, WebSocket, JSON-RPC and static frontend routes.
//
// 设计：JSON 类接口统一经声明式路由桥 jsonRpc.Bind 绑定到对应 RPC2 方法，
// 不再有 per-resource gin handler 层。仅二进制/流/重定向/特殊鉴权类接口保留为 REST handler。
func Register(r *gin.Engine) {
	r.Any("/ping", func(c *gin.Context) {
		c.String(200, "pong")
	})

	registerPublicRoutes(r)
	registerAgentRoutes(r)
	registerAdminRoutes(r)
	connectapi.Register(r)

	public.Static(r.Group("/"), func(handlers ...gin.HandlerFunc) {
		r.NoRoute(handlers...)
	})
}

// registerPublicRoutes 公开路由。JSON 读接口经 Bind 绑定到 public: 命名空间方法。
func registerPublicRoutes(r *gin.Engine) {
	installweb.RegisterCompleted(r)

	// 非 JSON / 特殊流程，保留 REST handler。
	r.POST("/api/login", public_api.Login)
	r.GET("/api/logout", public_api.Logout)
	r.GET("/api/oauth", public_api.OAuth)
	r.GET("/api/oauth_callback", public_api.OAuthCallback)
	r.GET("/api/mjpeg_live", public_api.MjpegLiveHandler)
	r.GET("/api/plugin/:short/*filepath", public_api.ServePluginFile)
	// /api/clients 是 WebSocket 端点（客户端发 "get"/"get <uuid>" 拉取在线列表与最新上报），
	// 非 JSON-RPC，保留为 WS handler。
	r.GET("/api/clients", api.GetClients)

	// JSON 接口 -> RPC2。
	r.GET("/api/me", jsonRpc.Bind("public:getMe", jsonRpc.WithRaw()))
	r.GET("/api/nodes", jsonRpc.Bind("public:getNodesInformation"))
	r.GET("/api/public", jsonRpc.Bind("public:getPublicSettings"))
	r.GET("/api/version", jsonRpc.Bind("public:getVersion"))
	r.GET("/api/recent/:uuid", jsonRpc.Bind("public:getClientRecentRecords", jsonRpc.WithPath("uuid")))
	r.GET("/api/records/load", jsonRpc.Bind("public:getRecordsByUUID", jsonRpc.WithQuery("uuid", "load_type", "hours")))
	r.GET("/api/records/ping", jsonRpc.Bind("public:getPingRecords", jsonRpc.WithQuery("uuid", "task_id", "hours")))
	r.GET("/api/task/ping", jsonRpc.Bind("public:getPublicPingTasks"))

	// JSON-RPC 直连入口。
	r.GET("/api/rpc2", jsonRpc.OnRpcRequest)
	r.POST("/api/rpc2", jsonRpc.OnRpcRequest)
}

// registerAgentRoutes agent（客户端）上报与拉取路由。
func registerAgentRoutes(r *gin.Engine) {
	// AutoDiscovery 注册使用独立的 Authorization key 鉴权，保留 REST handler。
	r.POST("/api/clients/register", client.RegisterClient)

	tokenAuthorized := r.Group("/api/clients", api.RequireRole(api.RoleAdmin, api.RoleClient))
	{
		// 上报类（WS / 原始流 / 兼容协议）保留 REST handler。
		tokenAuthorized.GET("/report", client.WebSocketReport)
		tokenAuthorized.POST("/uploadBasicInfo", client.UploadBasicInfo)
		tokenAuthorized.POST("/report", client.UploadReport)
		tokenAuthorized.GET("/v2/rpc", client.WebSocketV2RPC)
		tokenAuthorized.POST("/v2/rpc", client.UploadV2RPC)

		// Agent 侧遗留终端/远程通道。Connect 的 WebSSHService.AttachSession 已覆盖，
		// 仅在显式开启兼容开关时挂载。
		if legacyRemoteCompatEnabled() {
			tokenAuthorized.GET("/terminal", terminal.EstablishConnection)
			tokenAuthorized.GET("/remote", remote.EstablishAgent)
		}

		// JSON 接口 -> RPC2 (client: 命名空间)。
		tokenAuthorized.POST("/task/result", jsonRpc.Bind("client:taskResult", jsonRpc.WithRaw()))
		tokenAuthorized.GET("/ping/tasks", jsonRpc.Bind("client:getPingTasks", jsonRpc.WithRaw()))
		tokenAuthorized.POST("/ping/result", jsonRpc.Bind("client:uploadPingResult", jsonRpc.WithRaw()))
	}
}

// registerAdminRoutes 管理员路由。除二进制/流类外全部经 Bind 绑定到 admin: 命名空间方法。
func registerAdminRoutes(r *gin.Engine) {
	g := r.Group("/api/admin", api.RequireRole(api.RoleAdmin))
	// dashboard 读取已迁移到 komari.admin.v1.DashboardService。admin:getDashboard*
	// 方法仍在 /api/rpc2 上注册，供未改造的第三方主题使用，但不再有 REST 桥接路由。
	admin.RegisterPprofRoutes(g)
	admin.RegisterHistoryExportRoutes(g)

	// --- 二进制/流/重定向类，保留 REST handler ---
	g.GET("/download/backup", admin.DownloadBackup)
	g.POST("/upload/backup", admin.UploadBackup)
	g.GET("/test/geoip", jsonRpc.Bind("admin:testGeoip", jsonRpc.WithQuery("ip")))
	g.POST("/test/sendMessage", jsonRpc.Bind("admin:testSendMessage"))
	g.POST("/update/mmdb", admin.UpdateMmdbGeoIP)
	g.POST("/update/user", admin.UpdateUser)
	g.PUT("/update/favicon", admin.UploadFavicon)
	g.POST("/update/favicon", admin.DeleteFavicon)
	g.GET("/settings/https", admin.GetHTTPSSettings)
	g.POST("/settings/https", admin.UpdateHTTPSSettings)
	g.POST("/settings/https/reload", admin.ReloadHTTPSCertificate)

	// theme 含文件上传，保留 REST handler。
	theme := g.Group("/theme")
	{
		theme.PUT("/upload", admin.UploadTheme)
		theme.GET("/list", admin.ListThemes)
		theme.POST("/delete", admin.DeleteTheme)
		theme.GET("/set", admin.SetTheme)
		theme.POST("/update", admin.UpdateTheme)
		theme.POST("/import", admin.ImportTheme)
		theme.POST("/settings", admin.UpdateThemeSettings)
		theme.GET("/market/sources", admin.ListThemeMarketSources)
		theme.POST("/market/sources", admin.CreateThemeMarketSource)
		theme.PUT("/market/sources/:id", admin.UpdateThemeMarketSource)
		theme.DELETE("/market/sources/:id", admin.DeleteThemeMarketSource)
		theme.GET("/market/catalog", admin.ListThemeMarketCatalog)
		theme.POST("/market/install", admin.InstallThemeFromMarket)
	}

	// 2FA 含二维码 PNG / 敏感操作，保留 REST handler。
	twoFactor := g.Group("/2fa")
	{
		twoFactor.GET("/generate", admin.Generate2FA)
		twoFactor.POST("/enable", admin.Enable2FA)
		twoFactor.POST("/disable", api.RequireSensitive2FA(), admin.Disable2FA)
	}

	// oauth2 绑定走重定向，保留 REST handler。
	oauth2 := g.Group("/oauth2")
	{
		oauth2.GET("/bind", admin.BindingExternalAccount)
		oauth2.POST("/unbind", admin.UnbindExternalAccount)
	}

	// --- 以下全部 JSON -> RPC2 ---

	// 远程执行已由 komari.exec.v1.ExecutionService 承担（下发、观察、取消），
	// admin:getTasks / admin:exec 等方法仍在 /api/rpc2 上保留。

	// settings
	settings := g.Group("/settings")
	{
		settings.GET("/", jsonRpc.Bind("admin:getSettings"))
		settings.POST("/", jsonRpc.Bind("admin:editSettings"))
		settings.GET("/xtermjs", jsonRpc.Bind("admin:getXtermjsSettings"))
		settings.POST("/xtermjs", jsonRpc.Bind("admin:setXtermjsSettings", jsonRpc.WithMessage("settings saved")))
		settings.GET("/dashboard", jsonRpc.Bind("admin:getDashboardSettings"))
		settings.POST("/dashboard", jsonRpc.Bind("admin:setDashboardSettings", jsonRpc.WithMessage("settings saved")))
		settings.POST("/oidc", jsonRpc.Bind("admin:setOidcProvider"))
		settings.GET("/oidc", jsonRpc.Bind("admin:getOidcProvider", jsonRpc.WithQuery("provider")))
		settings.POST("/message-sender", jsonRpc.Bind("admin:setMessageSenderProvider"))
		settings.GET("/message-sender", jsonRpc.Bind("admin:getMessageSenderProvider", jsonRpc.WithQuery("provider")))
		settings.GET("/cloudflared", jsonRpc.Bind("admin:getCloudflaredStatus"))
		settings.POST("/cloudflared/start", jsonRpc.Bind("admin:startCloudflared"))
		settings.POST("/cloudflared/stop", jsonRpc.Bind("admin:stopCloudflared"))
		settings.POST("/cloudflared/remove-token", jsonRpc.Bind("admin:removeCloudflaredToken"))
	}

	// clients
	clientGroup := g.Group("/client")
	{
		// 浏览器侧遗留远程控制入口。Connect 的 WebSSHService.CreateSession /
		// WatchSession / SendSessionCommand 已覆盖且为前端唯一使用路径，
		// 仅在显式开启兼容开关时挂载。
		if legacyRemoteCompatEnabled() {
			clientGroup.POST("/remote/authorize", remote.Authorize)
			clientGroup.POST("/remote/session", remote.CreateSession)
			clientGroup.POST("/remote/session/cancel", remote.CancelSession)
			clientGroup.GET("/remote", remote.ConnectBrowser)
		}
		clientGroup.POST("/add", jsonRpc.Bind("admin:addClient", jsonRpc.WithFlat()))
		clientGroup.GET("/list", jsonRpc.Bind("admin:listClients", jsonRpc.WithRaw()))
		clientGroup.GET("/:uuid", jsonRpc.Bind("admin:getClient", jsonRpc.WithPath("uuid"), jsonRpc.WithRaw()))
		clientGroup.POST("/:uuid/edit", jsonRpc.Bind("admin:editClient", jsonRpc.WithPath("uuid")))
		clientGroup.POST("/:uuid/remove", jsonRpc.Bind("admin:removeClient", jsonRpc.WithPath("uuid")))
		clientGroup.GET("/:uuid/token", jsonRpc.Bind("admin:getClientToken", jsonRpc.WithPath("uuid"), jsonRpc.WithFlat()))
		clientGroup.GET("/:uuid/deployment-profile", jsonRpc.Bind("admin:getClientDeploymentProfile", jsonRpc.WithPath("uuid"), jsonRpc.WithRaw()))
		clientGroup.POST("/:uuid/deployment-profile", jsonRpc.Bind("admin:saveClientDeploymentProfile", jsonRpc.WithPath("uuid"), jsonRpc.WithRaw()))
		clientGroup.GET("/:uuid/traffic-calibration", admin.GetTrafficCalibration)
		clientGroup.POST("/:uuid/traffic-calibration", admin.UpdateTrafficCalibration)
		clientGroup.POST("/token/rotate", api.RequireSensitive2FA(), jsonRpc.Bind("admin:rotateClientToken"))
		clientGroup.POST("/order", jsonRpc.Bind("admin:orderClients"))
		if legacyRemoteCompatEnabled() {
			clientGroup.GET("/:uuid/terminal", api.RequireSensitive2FA(), terminal.RequestTerminal)
		}
	}

	// records、sessions、logs、clipboard、database 读写已迁移到
	// komari.admin.v1.MaintenanceService；对应 admin:* 方法仍在 /api/rpc2 上保留。

	pluginGroup := g.Group("/plugin")
	{
		pluginGroup.GET("/list", jsonRpc.Bind("admin:listPlugins"))
		pluginGroup.POST("/enabled", jsonRpc.Bind("admin:setPluginEnabled"))
		pluginGroup.GET("/logs", jsonRpc.Bind("admin:getPluginLogs", jsonRpc.WithQuery("short")))
		pluginGroup.POST("/install", admin.UploadPlugin)
		pluginGroup.GET("/market/sources", admin.ListPluginMarketSources)
		pluginGroup.POST("/market/sources", admin.CreatePluginMarketSource)
		pluginGroup.PUT("/market/sources/:id", admin.UpdatePluginMarketSource)
		pluginGroup.DELETE("/market/sources/:id", admin.DeletePluginMarketSource)
		pluginGroup.GET("/market/catalog", admin.ListPluginMarketCatalog)
		pluginGroup.POST("/market/install", admin.InstallPluginFromMarket)
		pluginGroup.POST("/delete", jsonRpc.Bind("admin:deletePlugin"))
		pluginGroup.GET("/configuration", jsonRpc.Bind("admin:getPluginConfiguration", jsonRpc.WithQuery("short")))
		pluginGroup.POST("/configuration", jsonRpc.Bind("admin:setPluginConfiguration"))
		pluginGroup.GET("/:short/*filepath", admin.ServePluginFile)
	}

	// notifications
	notificationGroup := g.Group("/notification")
	{
		notificationGroup.GET("/offline", jsonRpc.Bind("admin:listOfflineNotifications"))
		notificationGroup.POST("/offline/edit", jsonRpc.Bind("admin:editOfflineNotification"))
		notificationGroup.POST("/offline/enable", jsonRpc.Bind("admin:enableOfflineNotification"))
		notificationGroup.POST("/offline/disable", jsonRpc.Bind("admin:disableOfflineNotification"))
		loadAlert := notificationGroup.Group("/load")
		{
			loadAlert.GET("/", jsonRpc.Bind("admin:getAllLoadNotifications"))
			loadAlert.POST("/add", jsonRpc.Bind("admin:addLoadNotification"))
			loadAlert.POST("/delete", jsonRpc.Bind("admin:deleteLoadNotification"))
			loadAlert.POST("/edit", jsonRpc.Bind("admin:editLoadNotification"))
		}
		trafficReport := notificationGroup.Group("/traffic-report")
		{
			trafficReport.GET("/", jsonRpc.Bind("admin:listTrafficReportNotifications"))
			trafficReport.POST("/edit", jsonRpc.Bind("admin:editTrafficReportNotifications"))
			trafficReport.POST("/enable", jsonRpc.Bind("admin:enableTrafficReportNotifications"))
			trafficReport.POST("/disable", jsonRpc.Bind("admin:disableTrafficReportNotifications"))
			trafficReport.POST("/send-daily", jsonRpc.Bind("admin:sendDailyTrafficReport"))
		}
		pingLoss := notificationGroup.Group("/ping-loss")
		{
			pingLoss.GET("/", jsonRpc.Bind("admin:listPingLossNotifications"))
			pingLoss.POST("/add", jsonRpc.Bind("admin:addPingLossNotification"))
			pingLoss.POST("/edit", jsonRpc.Bind("admin:editPingLossNotifications"))
			pingLoss.POST("/batch", jsonRpc.Bind("admin:upsertPingLossNotifications"))
			pingLoss.POST("/delete", jsonRpc.Bind("admin:deletePingLossNotifications"))
		}
	}

	// ping tasks 已迁移到 komari.admin.v1.PingTaskService；
	// admin:*PingTask* 方法仍在 /api/rpc2 上保留。

	returnRoute := g.Group("/return-route")
	{
		returnRoute.GET("/", jsonRpc.Bind("admin:getReturnRouteOverview"))
		returnRoute.GET("/summary", jsonRpc.Bind("admin:getReturnRouteSummary"))
		returnRoute.POST("/tasks/query", jsonRpc.Bind("admin:queryReturnRouteTasks"))
		returnRoute.POST("/events/query", jsonRpc.Bind("admin:queryReturnRouteEvents"))
		returnRoute.POST("/add", jsonRpc.Bind("admin:addReturnRouteTask"))
		returnRoute.POST("/edit", jsonRpc.Bind("admin:editReturnRouteTask"))
		returnRoute.POST("/edit/batch", jsonRpc.Bind("admin:batchEditReturnRouteTasks"))
		returnRoute.POST("/delete", jsonRpc.Bind("admin:deleteReturnRouteTask"))
		returnRoute.POST("/probe", jsonRpc.Bind("admin:probeReturnRouteNow"))
		returnRoute.GET("/rules", jsonRpc.Bind("admin:getReturnRouteRules"))
		returnRoute.POST("/rules/reload", jsonRpc.Bind("admin:reloadReturnRouteRules"))
		returnRoute.POST("/rules/update", jsonRpc.Bind("admin:updateReturnRouteRules"))
		returnRoute.POST("/rules/refresh", jsonRpc.Bind("admin:refreshReturnRouteBGPRules"))
	}
}
