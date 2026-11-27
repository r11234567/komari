package plugin

import (
	"net/http"
	"path"
	"strings"
)

const legacyPasskeyPluginShort = "komari-passkey"

// IsRetiredPlugin reports whether an installed or catalog name is retired.
func IsRetiredPlugin(names ...string) bool {
	for _, name := range names {
		if strings.EqualFold(strings.TrimSpace(name), legacyPasskeyPluginShort) {
			return true
		}
	}
	return false
}

func isSensitivePluginHeader(name string, values []string, allowForeignAuth bool) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	switch lower {
	case "cookie", "proxy-authorization", "dpop",
		"x-csrf-token", "x-bind-ticket", "x-two-factor-code":
		return true
	case "authorization":
		if !allowForeignAuth {
			return true
		}
		for _, v := range values {
			lv := strings.ToLower(strings.TrimSpace(v))
			if strings.Contains(lv, "komari_pat_") || strings.Contains(lv, "komari-") {
				return true
			}
		}
		return false
	}
	return strings.HasPrefix(lower, "x-2fa-") ||
		strings.HasPrefix(lower, "x-two-factor-") ||
		strings.HasPrefix(lower, "x-komari-")
}

// sanitizedPluginHeaders removes session cookies, DPoP/step-up/workspace
// headers, and Komari/hook credentials before a host request enters the
// plugin JavaScript runtime. Plugin-owned HTTP routes may still receive foreign
// third-party Bearer credentials when allowForeignAuth is true and the request
// was not authenticated as a Komari API token or Agent.
func sanitizedPluginHeaders(src http.Header, allowForeignAuth bool) http.Header {
	if len(src) == 0 {
		return nil
	}
	dst := make(http.Header, len(src))
	for name, values := range src {
		if isSensitivePluginHeader(name, values, allowForeignAuth) {
			continue
		}
		dst[name] = append([]string(nil), values...)
	}
	return dst
}

func isReservedSessionCookieName(name string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	switch lower {
	case "session_token", "oauth_state", "oauth_return_to", "binding_external_account", "2fa_secret", "temp_key", "komari_sign_on_grant":
		return true
	}
	return strings.HasPrefix(lower, "komari_") ||
		strings.HasPrefix(lower, "__host-komari_") ||
		strings.HasPrefix(lower, "__secure-komari_")
}

func isRestrictedHookMutationHeader(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "authorization", "cookie", "host", "origin", "referer",
		"x-forwarded-for", "x-forwarded-proto", "x-forwarded-host",
		"forwarded", "x-real-ip",
		"x-komari-credential-authorization", "x-komari-step-up", "dpop":
		return true
	default:
		return false
	}
}

// stripReservedSetCookies prevents plugin response hooks from issuing or
// overwriting Komari session, bind, or security cookies.
func stripReservedSetCookies(header http.Header) {
	if len(header) == 0 {
		return
	}
	for key, values := range header {
		if !strings.EqualFold(key, "Set-Cookie") {
			continue
		}
		kept := make([]string, 0, len(values))
		for _, raw := range values {
			cookieName, _, _ := strings.Cut(raw, "=")
			if isReservedSessionCookieName(cookieName) {
				continue
			}
			kept = append(kept, raw)
		}
		delete(header, key)
		for _, v := range kept {
			header.Add("Set-Cookie", v)
		}
	}
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

// Sensitive control-plane, authentication, login, admin UI, workspace, Connect
// RPC, agent control, and file transfer routes must never enter generic plugin
// HTTP/WS hooks, HTML injection, or plugin-owned HTTP/static routes. Public
// storefront pages, custom plugin routes, and legacy public RPC2 paths keep
// their previous scopes.
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
		"/api/me",
		"/api/oauth",
		"/api/oauth_callback",
		"/api/bind/ticket",
		"/api/public/bind/ticket",
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
	if strings.HasPrefix(p, "/api/clients/") {
		return true
	}
	return strings.HasPrefix(p, "/api/preview/client/") && strings.Contains(p, "/file/")
}

// pluginAllowedOnPath enforces the unconditional control-plane and login
// boundary for all plugins.
func pluginAllowedOnPath(p, _ string) bool {
	return !protectedWorkspacePath(p)
}
