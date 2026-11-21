package security

import (
	"log"
	"net/http"
	"net/url"
	"strings"

	"github.com/komari-monitor/komari/pkg/requestmeta"
)

func SplitAllowlist(raw string) []string {
	parts := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\r'
	})
	entries := make([]string, 0, len(parts))
	for _, part := range parts {
		entry := strings.TrimSpace(part)
		if entry != "" {
			entries = append(entries, entry)
		}
	}
	return entries
}

// OriginMatchesRequest checks the complete origin against the shared explicit
// proxy policy. Neither a private peer nor a matching hostname implies trust.
func OriginMatchesRequest(origin string, r *http.Request) bool {
	requested, err := requestmeta.NormalizeOrigin(origin)
	if err != nil {
		return false
	}
	actual, err := requestmeta.Origin(r)
	return err == nil && requested == actual
}

func OriginInAllowlist(origin, rawAllowlist string) bool {
	normalizedOrigin, _, ok := normalizeOrigin(origin)
	if !ok {
		return false
	}
	allowlist, err := requestmeta.NormalizeAllowlist(rawAllowlist, true)
	if err != nil {
		return false
	}
	for _, entry := range SplitAllowlist(allowlist) {
		if entry == "*" {
			return true
		}
		normalizedEntry, _, ok := normalizeOrigin(entry)
		if ok && normalizedEntry == normalizedOrigin {
			return true
		}
	}
	return false
}

func WarnLegacyOrigins(kind, raw string) {
	for _, entry := range SplitAllowlist(raw) {
		if entry == "*" {
			log.Printf("[SECURITY] %s allowed origins contains wildcard '*': replace it with explicit http(s):// origins", kind)
			return
		}
		if !strings.Contains(entry, "://") {
			log.Printf("[SECURITY] %s allowed origins entry %q has no scheme; configure full http(s):// origins", kind, entry)
			return
		}
	}
}

func normalizeOrigin(raw string) (string, string, bool) {
	normalized, err := requestmeta.NormalizeOrigin(strings.TrimSpace(raw))
	if err != nil {
		return "", "", false
	}
	parsed, err := url.Parse(normalized)
	if err != nil || parsed.Host == "" {
		return "", "", false
	}
	return normalized, parsed.Host, true
}

// IsRequestSecure reports whether the request arrived over TLS directly or over
// HTTPS terminated by an explicitly trusted reverse proxy.
func IsRequestSecure(r *http.Request) bool {
	if r == nil {
		return false
	}
	if r.TLS != nil {
		return true
	}
	return requestmeta.ForwardedHTTPS(r)
}
