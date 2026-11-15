package security

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/komari-monitor/komari/pkg/config"
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

func OriginMatchesHost(origin, host string) bool {
	_, originHost, ok := normalizeOrigin(origin)
	return ok && strings.EqualFold(originHost, host)
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
	normalizedOrigin, originHost, ok := normalizeOrigin(origin)
	if !ok {
		return false
	}
	for _, entry := range SplitAllowlist(rawAllowlist) {
		if entry == "*" {
			return true
		}
		if strings.Contains(entry, "://") {
			normalizedEntry, _, ok := normalizeOrigin(entry)
			if ok && strings.EqualFold(normalizedEntry, normalizedOrigin) {
				return true
			}
			continue
		}
		if strings.EqualFold(entry, originHost) {
			return true
		}
	}
	return false
}

func IsAPIKeyRequest(r *http.Request) bool {
	if authenticated, ok := r.Context().Value(apiKeyContextKey{}).(bool); ok {
		return authenticated
	}
	apiKeyConfig, err := config.GetAs[string](config.ApiKeyKey, "")
	if err != nil || apiKeyConfig == "" || len(apiKeyConfig) < 12 {
		return false
	}
	return APIKeyMatches(r.Header.Get("Authorization"), apiKeyConfig)
}

func IsAuthorizationPreflight(r *http.Request) bool {
	if r.Method != http.MethodOptions {
		return false
	}
	for _, header := range strings.Split(r.Header.Get("Access-Control-Request-Headers"), ",") {
		if strings.EqualFold(strings.TrimSpace(header), "authorization") {
			return true
		}
	}
	return false
}

func normalizeOrigin(raw string) (string, string, bool) {
	normalized, err := requestmeta.NormalizeOrigin(raw)
	if err != nil {
		return "", "", false
	}
	parsed, _ := url.Parse(normalized)
	return normalized, parsed.Host, true
}
