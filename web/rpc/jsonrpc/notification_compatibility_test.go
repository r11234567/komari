package jsonrpc

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/pkg/rpc"
	"github.com/komari-monitor/komari/utils/messageSender"
)

func TestSendNotificationAcceptsOfficialAndLegacyPayloads(t *testing.T) {
	at := time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC)
	// Isolated synthetic notification data; never send real OTPs from tests.
	event := map[string]any{"event": "Login", "time": at.Format(time.RFC3339), "message": "synthetic notification", "emoji": "test"}
	want := models.EventMessage{Kind: messageSender.KindLogin, Event: "Login", Time: at, Message: "synthetic notification", Emoji: "test"}
	for _, tc := range []struct {
		name   string
		params any
		want   models.EventMessage
	}{
		{"official named envelope", map[string]any{"event": event}, want},
		{"official positional event", []any{event}, want},
		{"fork flat event", event, want},
		{"fork positional event", []any{"login", "Login", nil, at.Format(time.RFC3339), "synthetic notification", "test"}, want},
		{"explicit kind", map[string]any{"event": map[string]any{"kind": "online", "event": "Custom", "message": "synthetic recovery"}}, models.EventMessage{Kind: "online", Event: "Custom", Message: "synthetic recovery"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := bindNotificationEvent(&rpc.JsonRpcRequest{Params: tc.params})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("decoded notification = %#v; want %#v", got, tc.want)
			}
		})
	}
}

func TestSendNotificationRejectsMalformedPayloadWithoutEchoingBody(t *testing.T) {
	const privateMarker = "synthetic-private-request-data"
	for _, params := range []any{
		nil, map[string]any{}, map[string]any{"event": nil},
		map[string]any{"event": 123}, map[string]any{"event": true},
		map[string]any{"event": " "},
		map[string]any{"event": map[string]any{"message": privateMarker}},
		map[string]any{"event": map[string]any{"event": map[string]any{"event": "Login"}}},
		map[string]any{"event": map[string]any{"event": "Login", "time": privateMarker}},
		map[string]any{"event": "Login", "time": privateMarker},
	} {
		_, err := adminSendNotification(context.Background(), &rpc.JsonRpcRequest{Params: params})
		if err == nil || err.Code != rpc.InvalidParams {
			t.Fatalf("malformed parameters returned %v; want InvalidParams", err)
		}
		if strings.Contains(err.Message, privateMarker) || err.Data != nil {
			t.Fatalf("parameter error exposed request data: %#v", err)
		}
	}
	if _, err := bindNotificationEvent(nil); err == nil {
		t.Fatal("nil request accepted")
	}
}

func TestSendNotificationKeepsAdminPermission(t *testing.T) {
	const method = "admin:sendNotification"
	if !rpc.CheckPermission(rpc.RoleAdmin, method) {
		t.Fatal("approved system RPC calls must retain admin access")
	}
	for _, role := range []string{rpc.RoleGuest, rpc.RoleClient} {
		if rpc.CheckPermission(role, method) {
			t.Fatalf("notification sending allowed for %s", role)
		}
	}
	if rpc2TransportAllowed(method) {
		t.Fatal("notification management must stay off the public RPC2 transport")
	}
}
