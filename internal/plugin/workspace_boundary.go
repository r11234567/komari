package plugin

import (
	"path"
	"strings"
)

const legacyPasskeyPluginShort = "komari-passkey"

func normalizeBoundaryPath(raw string) string {
	raw = strings.ToLower(strings.TrimSpace(raw))
	if raw == "" {
		return ""
	}
	if idx := strings.IndexByte(raw, '?'); idx >= 0 {
		raw = raw[:idx]
	}
	if raw == "" {
		return ""
	}
	if !strings.HasPrefix(raw, "/") {
		raw = "/" + raw
	}
	cleaned := path.Clean(raw)
	if cleaned != "/" {
		cleaned = strings.TrimRight(cleaned, "/")
	}
	return cleaned
}

// Sensitive control-plane, authentication, login, admin UI, workspace, Connect
// RPC, and file transfer routes must never enter generic plugin HTTP/WS hooks
// or HTML injection. Public storefront pages, custom plugin routes, and legacy
// public RPC2 paths keep their previous scopes.
func protectedWorkspacePath(p string) bool {
	p = normalizeBoundaryPath(p)
	if p == "" {
		return false
	}
	for _, exact := range []string{
		"/login",
		"/admin",
		"/terminal",
		"/bind",
		"/bind-login",
		"/enroll",
		"/install",
		"/manage",
		"/database-recovery",
		"/api/login",
		"/api/logout",
		"/api/oauth",
		"/api/oauth_callback",
	} {
		if p == exact || strings.HasPrefix(p, exact+"/") {
			return true
		}
	}
	for _, prefix := range []string{
		"/komari.",
		"/api/admin",
		"/api/clients/transfer",
	} {
		if p == prefix || strings.HasPrefix(p, prefix+"/") || (strings.HasSuffix(prefix, ".") && strings.HasPrefix(p, prefix)) {
			return true
		}
	}
	return strings.HasPrefix(p, "/api/preview/client/") && strings.Contains(p, "/file/")
}

// pluginAllowedOnPath enforces the unconditional control-plane and login
// boundary for all plugins.
func pluginAllowedOnPath(p, _ string) bool {
	return !protectedWorkspacePath(p)
}
