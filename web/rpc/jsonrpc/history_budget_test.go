package jsonrpc

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/komari-monitor/komari/database/metricstore"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/pkg/config"
	"github.com/komari-monitor/komari/pkg/rpc"
	"github.com/komari-monitor/komari/web/connection"
)

func TestLegacyHistoryWindowValidation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		handler rpc.Handler
		params  map[string]any
	}{
		{"load too long", publicGetRecordsByUUID, map[string]any{"uuid": "test", "hours": "169"}},
		{"load duration overflow", publicGetRecordsByUUID, map[string]any{"uuid": "test", "hours": "9223372036854775807"}},
		{"ping negative", publicGetPingRecords, map[string]any{"uuid": "test", "hours": "-1"}},
		{"ping invalid type", publicGetPingRecords, map[string]any{"uuid": "test", "hours": true}},
		{"ping malformed", publicGetPingRecords, map[string]any{"uuid": "test", "hours": "bad"}},
		{"common duration overflow", getRecords, map[string]any{"hours": int64(9223372036854775807)}},
		{"common reversed", getRecords, map[string]any{"start": "2026-01-02T00:00:00Z", "end": "2026-01-01T00:00:00Z"}},
		{"common explicit too long", getRecords, map[string]any{"start": "2026-01-01T00:00:00Z", "end": "2026-01-09T00:00:00Z"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tc.handler(context.Background(), rpc.NewRequest(1, "test", tc.params)); err == nil || err.Code != rpc.InvalidParams {
				t.Fatalf("expected invalid params, got %+v", err)
			}
		})
	}
	for _, tc := range []struct {
		input string
		want  time.Duration
	}{
		{"", 4 * time.Hour},
		{"1", time.Hour},
		{"168", 7 * 24 * time.Hour},
	} {
		got, err := parsePublicHistoryHours(tc.input)
		if err != nil || got != tc.want {
			t.Fatalf("parsePublicHistoryHours(%q) = %v, %+v; want %v", tc.input, got, err, tc.want)
		}
	}
}

func TestLongHistorySaturationLeavesNormalQueryAvailable(t *testing.T) {
	ctx := context.Background()
	if err := config.Set(metricstore.MetricDBDSNKey, filepath.Join(t.TempDir(), "metrics.db")); err != nil {
		t.Fatalf("configure metrics: %v", err)
	}
	if err := metricstore.Reload(ctx); err != nil {
		t.Fatalf("reload metrics: %v", err)
	}

	for range cap(longCompatibilityHistorySlots) {
		release, err := acquireLongCompatibilityHistory(ctx, 7*24*time.Hour)
		if err != nil {
			t.Fatalf("reserve a long query slot: %v", err)
		}
		t.Cleanup(release)
	}
	req := rpc.NewRequest(1, "public:getRecordsByUUID", map[string]any{
		"uuid": "normal-history-node", "load_type": "cpu", "hours": "168",
	})
	if _, err := publicGetRecordsByUUID(ctx, req); err == nil || err.Code != rpc.Unavailable {
		t.Fatalf("long query should be rejected when slots are full: %+v", err)
	}

	req = rpc.NewRequest(1, "public:getRecordsByUUID", map[string]any{
		"uuid": "normal-history-node", "load_type": "cpu", "hours": "24",
	})
	if err := metricstore.WritePingRecord(ctx, models.PingRecord{
		Client: "normal-history-node", TaskId: 1, Time: time.Now().Add(-time.Minute), Value: 10,
	}); err != nil {
		t.Fatalf("write ping fixture: %v", err)
	}
	if _, err := publicGetRecordsByUUID(ctx, req); err != nil {
		t.Fatalf("normal history request with long slots full: %+v", err)
	}
	ping := rpc.NewRequest(2, "public:getPingRecords", map[string]any{
		"uuid": "normal-history-node", "task_id": "1", "hours": "1",
	})
	if _, err := publicGetPingRecords(ctx, ping); err != nil {
		t.Fatalf("normal ping request with long slots full: %+v", err)
	}
}

func TestLegacyHistoryRespondsToCancelledContext(t *testing.T) {
	for _, tc := range []struct {
		handler rpc.Handler
		params  map[string]any
	}{
		{publicGetRecordsByUUID, map[string]any{"uuid": "test"}},
		{publicGetPingRecords, map[string]any{"uuid": "test"}},
		{getRecords, map[string]any{"uuid": "test"}},
	} {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := tc.handler(ctx, rpc.NewRequest(1, "test", tc.params)); err == nil || err.Code != rpc.Cancelled {
			t.Fatalf("expected cancelled RPC error, got %+v", err)
		}
	}
}

func TestCompatibilityWebSocketReaderDetectsDisconnectWithPendingRequest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	queue := make(chan compatibilityWSRequest, 1)
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		safe := connection.NewSafeConn(conn)
		defer safe.Close()
		defer cancel()
		readCompatibilityWSRequests(ctx, safe, queue)
	}))
	t.Cleanup(server.Close)

	socket, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer socket.Close()
	if err := socket.WriteJSON(rpc.NewRequest(1, "public:getPingRecords", nil)); err != nil {
		t.Fatal(err)
	}
	select {
	case pending := <-queue:
		if pending.request.Method != "public:getPingRecords" {
			t.Fatalf("unexpected request: %+v", pending)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("did not receive a request")
	}
	if err := socket.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("reader did not detect disconnect during the pending request")
	}
}
