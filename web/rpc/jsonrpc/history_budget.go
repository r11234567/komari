package jsonrpc

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/komari-monitor/komari/pkg/rpc"
)

const maxCompatibilityHistoryHours = 7 * 24

func compatibilityHistoryHours(hours int) (time.Duration, *rpc.JsonRpcError) {
	if hours < 1 || hours > maxCompatibilityHistoryHours {
		return 0, rpc.MakeError(rpc.InvalidParams, fmt.Sprintf("hours must be between 1 and %d", maxCompatibilityHistoryHours), nil)
	}
	return time.Duration(hours) * time.Hour, nil
}

func parsePublicHistoryHours(value string) (time.Duration, *rpc.JsonRpcError) {
	if value == "" {
		return 4 * time.Hour, nil
	}
	hours, err := strconv.Atoi(value)
	if err != nil {
		return 0, rpc.MakeError(rpc.InvalidParams, "Invalid hours parameter", nil)
	}
	return compatibilityHistoryHours(hours)
}

// Long compatibility requests get their own ceiling even when the optional
// per-source request rate limiter is disabled.
var longCompatibilityHistorySlots = make(chan struct{}, 2)

func acquireLongCompatibilityHistory(ctx context.Context, window time.Duration) (func(), *rpc.JsonRpcError) {
	if err := ctx.Err(); err != nil {
		return nil, metricQueryError(err, "Failed to start history query")
	}
	if window <= 24*time.Hour {
		return func() {}, nil
	}
	select {
	case longCompatibilityHistorySlots <- struct{}{}:
		return func() { <-longCompatibilityHistorySlots }, nil
	default:
		return nil, rpc.MakeError(rpc.Unavailable, "long history queries are busy; retry shortly", nil)
	}
}
