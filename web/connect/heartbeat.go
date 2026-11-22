package connectapi

import (
	"strings"
	"time"
)

var streamHeartbeatInterval = 25 * time.Second

func extractAuthSchemeToken(header string) string {
	trimmed := strings.TrimSpace(header)
	if len(trimmed) > 7 && strings.EqualFold(trimmed[:7], "Bearer ") {
		return strings.TrimSpace(trimmed[7:])
	}
	if len(trimmed) > 5 && strings.EqualFold(trimmed[:5], "DPoP ") {
		return strings.TrimSpace(trimmed[5:])
	}
	return ""
}
