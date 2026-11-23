package connectapi

import (
	"context"
	"net/http"
	"strings"
	"time"
)

var streamHeartbeatInterval = 25 * time.Second

const streamHeartbeatHeader = "X-Komari-Stream-Heartbeat"

// These streams send empty protobuf frames only to clients that opt in.
func heartbeatTicks(headers http.Header) (<-chan time.Time, func()) {
	if headers.Get(streamHeartbeatHeader) != "1" {
		return nil, func() {}
	}
	ticker := time.NewTicker(streamHeartbeatInterval)
	return ticker.C, ticker.Stop
}

func waitForWatchPoll(ctx context.Context, interval time.Duration, ticks <-chan time.Time, heartbeat func() error) error {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticks:
			if err := heartbeat(); err != nil {
				return err
			}
		case <-timer.C:
			return nil
		}
	}
}

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
