package requestmeta

import (
	"fmt"
	"strings"
)

// NormalizeAllowlist validates new saves strictly. Existing settings can be
// read in legacy mode: hostname grants retain both schemes, and an explicit
// old wildcard remains visible until the operator replaces it. Never silently
// turn an upgrade into either a broader grant or a deny-all policy.
func NormalizeAllowlist(raw string, legacy bool) (string, error) {
	var result []string
	for _, part := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' }) {
		entry := strings.TrimSpace(part)
		if entry == "" {
			continue
		}
		if legacy && entry == "*" {
			result = append(result, entry)
			continue
		}
		if legacy {
			entry = strings.TrimSuffix(entry, "/")
		}
		if legacy && !strings.Contains(entry, "://") {
			httpOrigin, err := NormalizeOrigin("http://" + entry)
			if err != nil {
				return "", fmt.Errorf("invalid legacy Origin %q: %w", entry, err)
			}
			httpsOrigin, err := NormalizeOrigin("https://" + entry)
			if err != nil {
				return "", err
			}
			result = append(result, httpOrigin, httpsOrigin)
			continue
		}
		normalized, err := NormalizeOrigin(entry)
		if err != nil {
			return "", fmt.Errorf("Origin %q must include http:// or https:// and an optional port; wildcards are not accepted for new settings", entry)
		}
		result = append(result, normalized)
	}
	return strings.Join(result, ","), nil
}
