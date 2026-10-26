package jsonrpc

import (
	"context"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/pkg/rpc"
	"github.com/komari-monitor/komari/utils/messageSender"
	"github.com/stretchr/testify/require"
	"testing"
)

type routeConfigChannel struct{}

func (*routeConfigChannel) Send(context.Context, messageSender.Notification, map[string]any) error {
	return nil
}
func (*routeConfigChannel) Unload() error { return nil }

func TestNotificationSettingsExposePluginFieldsAndMaskSecrets(t *testing.T) {
	require.NoError(t, messageSender.RegisterNotificationChannel("route-config-test", models.Configuration{Data: []models.ManagedThemeConfigurationItem{
		{Key: "enabled", Type: "switch", Default: false}, {Key: "password", Type: "string", Default: ""},
	}}, &routeConfigChannel{}))
	t.Cleanup(func() { _ = messageSender.UnregisterNotificationChannel("route-config-test") })
	fields := senderConfigurations()["route-config-test"]
	require.Len(t, fields, 2)
	require.Equal(t, "bool", fields[0].Type)
	require.Equal(t, "false", fields[0].Default)
	require.True(t, getSenderSecretFields("route-config-test")["password"])
}
func TestNotificationSettingsRejectInvalidWindowsAndKeepAdminBoundary(t *testing.T) {
	for _, value := range []any{0, 3601, 1.5, "5"} {
		_, err := adminEditSettings(context.Background(), &rpc.JsonRpcRequest{Params: map[string]any{"notification_digest_seconds": value}})
		require.NotNil(t, err)
		require.Equal(t, rpc.InvalidParams, err.Code)
	}
	for _, name := range []string{"admin:editSettings", "admin:listNotificationChannels", "admin:setNotificationChannelConfiguration", "admin:testSendMessage", "admin:sendNotification"} {
		require.True(t, rpc.CheckPermission(rpc.RoleAdmin, name), name)
		require.False(t, rpc.CheckPermission(rpc.RoleGuest, name), name)
	}
}
