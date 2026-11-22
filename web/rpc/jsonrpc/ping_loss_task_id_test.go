package jsonrpc

import (
	"strconv"
	"testing"

	"github.com/komari-monitor/komari/pkg/rpc"
)

func TestPingLossTaskIDPreservesDecimalAcrossJSONBridge(t *testing.T) {
	if strconv.IntSize < 64 {
		t.Skip("fixture requires a platform with 64-bit task IDs")
	}
	req := &rpc.JsonRpcRequest{Params: map[string]any{"notifications": []any{
		map[string]any{"task_id_decimal": "9007199254740993"},
		map[string]any{"task_id": float64(42)},
	}}}
	rules, err := bindPingLossNotifications(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 2 || uint64(rules[0].TaskId) != uint64(9007199254740993) || rules[1].TaskId != 42 {
		t.Fatalf("unexpected task IDs: %+v", rules)
	}
	for _, fields := range []map[string]any{
		{"task_id_decimal": "18446744073709551616"},
		{"task_id_decimal": "43", "task_id": float64(42)},
	} {
		req.Params = map[string]any{"notifications": []any{fields}}
		if _, err := bindPingLossNotifications(req); err == nil {
			t.Error("accepted an overflowing or conflicting task ID")
		}
	}
}
