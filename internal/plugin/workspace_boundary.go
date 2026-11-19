package plugin

import (
	"os"
	"path"
	"strings"
)

const legacyPasskeyPluginShort = "komari-passkey"

// legacyPasskeyLoginCompatEnabled gates the temporary P1 compatibility
// exception allowing only the installed komari-passkey plugin to inject/hook
// the /login page until native Passkey login replaces it in P2.
func legacyPasskeyLoginCompatEnabled() bool {
	return !strings.EqualFold(strings.TrimSpace(os.Getenv("KOMARI_DISABLE_LEGACY_LOGIN_PLUGIN_COMPAT")), "true")
}

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

// isLegacyLoginPagePath matches ONLY the main login entry (/login or /login/),
// never trusted verification sub-routes such as /login/verify.
func isLegacyLoginPagePath(p string) bool {
	return normalizeBoundaryPath(p) == "/login"
}

// Sensitive control-plane, authentication, admin UI, workspace, Connect RPC,
// and file transfer routes must never enter generic plugin HTTP/WS hooks or
// HTML injection. Public storefront pages, custom plugin routes, and legacy
// public RPC2 paths keep their previous scopes.
func protectedWorkspacePath(p string) bool {
	p = normalizeBoundaryPath(p)
	if p == "" {
		return false
	}
	for _, exact := range []string{
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
	// Any sub-route under /login (e.g. /login/verify) is hard-protected for all
	// plugins, including komari-passkey.
	if strings.HasPrefix(p, "/login/") {
		return true
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

// pluginAllowedOnPath enforces the hard control-plane boundary for all plugins
// and restricts /login (and /login/) to the temporary komari-passkey
// compatibility exception.
func pluginAllowedOnPath(p, short string) bool {
	if protectedWorkspacePath(p) {
		return false
	}
	if isLegacyLoginPagePath(p) {
		return legacyPasskeyLoginCompatEnabled() && short == legacyPasskeyPluginShort
	}
	return true
}
