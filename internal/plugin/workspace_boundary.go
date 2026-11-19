package plugin

import "strings"

// Sensitive control-plane, authentication, admin UI, workspace, Connect RPC,
// and file transfer routes must never enter generic plugin HTTP/WS hooks or
// HTML injection. Public storefront pages, custom plugin routes, and legacy
// public RPC2 paths keep their previous scopes.
func protectedWorkspacePath(path string) bool {
	path = strings.ToLower(strings.TrimSpace(path))
	if path == "" {
		return false
	}
	if idx := strings.IndexByte(path, '?'); idx >= 0 {
		path = path[:idx]
	}
	for _, exact := range []string{"/admin", "/terminal", "/bind", "/login", "/bind-login", "/api/login", "/api/logout", "/api/oauth", "/api/oauth_callback"} {
		if path == exact {
			return true
		}
	}
	for _, prefix := range []string{
		"/admin/",
		"/terminal/",
		"/bind/",
		"/komari.",
		"/api/admin/",
		"/api/oauth/",
		"/api/clients/transfer/",
	} {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return strings.HasPrefix(path, "/api/preview/client/") && strings.Contains(path, "/file/")
}
