package plugin

import (
	"os"
	"strings"
)

const legacyPasskeyPluginShort = "komari-passkey"

// legacyPasskeyLoginCompatEnabled gates the temporary P1 compatibility
// exception allowing only the installed komari-passkey plugin to inject/hook
// the /login page until native Passkey login replaces it in P2.
func legacyPasskeyLoginCompatEnabled() bool {
	return !strings.EqualFold(strings.TrimSpace(os.Getenv("KOMARI_DISABLE_LEGACY_LOGIN_PLUGIN_COMPAT")), "true")
}

func normalizeBoundaryPath(path string) string {
	path = strings.ToLower(strings.TrimSpace(path))
	if idx := strings.IndexByte(path, '?'); idx >= 0 {
		path = path[:idx]
	}
	return path
}

func isLegacyLoginPagePath(path string) bool {
	return normalizeBoundaryPath(path) == "/login"
}

// Sensitive control-plane, authentication, admin UI, workspace, Connect RPC,
// and file transfer routes must never enter generic plugin HTTP/WS hooks or
// HTML injection. Public storefront pages, custom plugin routes, and legacy
// public RPC2 paths keep their previous scopes.
func protectedWorkspacePath(path string) bool {
	path = normalizeBoundaryPath(path)
	if path == "" {
		return false
	}
	for _, exact := range []string{"/admin", "/terminal", "/bind", "/bind-login", "/api/login", "/api/logout", "/api/oauth", "/api/oauth_callback"} {
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

// pluginAllowedOnPath enforces the hard control-plane boundary for all plugins
// and restricts /login to the temporary komari-passkey compatibility exception.
func pluginAllowedOnPath(path, short string) bool {
	if protectedWorkspacePath(path) {
		return false
	}
	if isLegacyLoginPagePath(path) {
		return legacyPasskeyLoginCompatEnabled() && short == legacyPasskeyPluginShort
	}
	return true
}
