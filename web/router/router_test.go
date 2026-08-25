package router

import (
	"testing"

	"github.com/gin-gonic/gin"
)

func TestRegisterDoesNotExposeHTTPSCertificateUpload(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	Register(engine)

	for _, route := range engine.Routes() {
		if route.Path == "/api/admin/settings/https/upload" {
			t.Fatalf("removed HTTPS certificate upload route is still registered: %s %s", route.Method, route.Path)
		}
	}
}

// legacyRemoteCompatRoutes are the pre-Connect terminal and remote control
// entry points. Connect WebSSH replaces all of them in production.
var legacyRemoteCompatRoutes = []string{
	"/api/clients/terminal",
	"/api/clients/remote",
	"/api/admin/client/remote",
	"/api/admin/client/remote/authorize",
	"/api/admin/client/remote/session",
	"/api/admin/client/remote/session/cancel",
	"/api/admin/client/:uuid/terminal",
}

func registeredPaths(t *testing.T) map[string]struct{} {
	t.Helper()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	Register(engine)
	paths := make(map[string]struct{})
	for _, route := range engine.Routes() {
		paths[route.Path] = struct{}{}
	}
	return paths
}

// TestDashboardRestBridgeIsRemoved keeps the dashboard on
// komari.admin.v1.DashboardService. The admin:getDashboard* RPC2 methods stay
// registered for unconverted themes, but must not regain a REST bridge.
func TestDashboardRestBridgeIsRemoved(t *testing.T) {
	paths := registeredPaths(t)
	for _, path := range []string{
		"/api/admin/dashboard",
		"/api/admin/dashboard/charts",
		"/api/admin/dashboard/alerts",
	} {
		if _, found := paths[path]; found {
			t.Fatalf("dashboard REST bridge %s is registered again", path)
		}
	}
}

// TestMaintenanceRestBridgeIsRemoved keeps sessions, audit logs, the clipboard,
// record retention and database maintenance on
// komari.admin.v1.MaintenanceService.
func TestMaintenanceRestBridgeIsRemoved(t *testing.T) {
	paths := registeredPaths(t)
	for _, path := range []string{
		"/api/admin/session/get",
		"/api/admin/session/remove",
		"/api/admin/session/remove/all",
		"/api/admin/logs",
		"/api/admin/clipboard",
		"/api/admin/clipboard/:id",
		"/api/admin/clipboard/remove",
		"/api/admin/clipboard/:id/remove",
		"/api/admin/record/clear",
		"/api/admin/record/clear/all",
		"/api/admin/database/size",
		"/api/admin/database/vacuum",
	} {
		if _, found := paths[path]; found {
			t.Fatalf("maintenance REST bridge %s is registered again", path)
		}
	}
}

// TestMigratedAdminRestBridgesAreRemoved covers the admin domains whose REST
// bridges were retired in favour of a Connect service: remote execution moved
// to komari.exec.v1.ExecutionService and probe tasks to
// komari.admin.v1.PingTaskService.
func TestMigratedAdminRestBridgesAreRemoved(t *testing.T) {
	paths := registeredPaths(t)
	for _, path := range []string{
		"/api/admin/task/all",
		"/api/admin/task/exec",
		"/api/admin/task/:task_id",
		"/api/admin/task/:task_id/result",
		"/api/admin/task/:task_id/result/:uuid",
		"/api/admin/task/client/:uuid",
		"/api/admin/ping/",
		"/api/admin/ping/add",
		"/api/admin/ping/delete",
		"/api/admin/ping/edit",
		"/api/admin/ping/order",
	} {
		if _, found := paths[path]; found {
			t.Fatalf("migrated REST bridge %s is registered again", path)
		}
	}
}

func TestLegacyRemoteRoutesAreUnmountedByDefault(t *testing.T) {
	paths := registeredPaths(t)
	for _, path := range legacyRemoteCompatRoutes {
		if _, found := paths[path]; found {
			t.Fatalf("legacy remote route %s must stay unmounted without KOMARI_LEGACY_REMOTE_COMPAT", path)
		}
	}
}

func TestLegacyRemoteRoutesMountWhenCompatEnabled(t *testing.T) {
	t.Setenv("KOMARI_LEGACY_REMOTE_COMPAT", "true")
	paths := registeredPaths(t)
	for _, path := range legacyRemoteCompatRoutes {
		if _, found := paths[path]; !found {
			t.Fatalf("legacy remote route %s must mount when KOMARI_LEGACY_REMOTE_COMPAT is enabled", path)
		}
	}
}
