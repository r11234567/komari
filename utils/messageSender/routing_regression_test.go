package messageSender

import (
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/pkg/config"
	"github.com/stretchr/testify/require"
	"math"
	"testing"
	"time"
)

func TestRouteMigrationPreservesExplicitEmptyAndSavedPlugin(t *testing.T) {
	oldRoutes, _ := config.GetAs[map[string][]string](config.NotificationRoutesKey, map[string][]string{})
	oldMigrated, _ := config.GetAs[bool](config.NotificationRoutesMigratedKey, false)
	oldMethod, _ := config.GetAs[string](config.NotificationMethodKey, "none")
	t.Cleanup(func() {
		_ = config.SetMany(map[string]any{config.NotificationRoutesKey: oldRoutes, config.NotificationRoutesMigratedKey: oldMigrated, config.NotificationMethodKey: oldMethod})
	})
	previous := providerConfigured
	providerConfigured = func(name string) (bool, error) { return name == "saved-plugin", nil }
	t.Cleanup(func() { providerConfigured = previous })
	require.NoError(t, config.SetMany(map[string]any{config.NotificationRoutesKey: SeedRoutes(""), config.NotificationRoutesMigratedKey: false, config.NotificationMethodKey: "saved-plugin"}))
	require.NoError(t, EnsureRoutes())
	routes, err := config.GetAs[map[string][]string](config.NotificationRoutesKey)
	require.NoError(t, err)
	require.Equal(t, []string{"saved-plugin"}, routes[KindOffline])
	require.NoError(t, SaveSettings(map[string]any{config.NotificationRoutesKey: SeedRoutes(""), config.NotificationRoutesMigratedKey: true}))
	require.NoError(t, EnsureRoutes())
	routes, err = config.GetAs[map[string][]string](config.NotificationRoutesKey)
	require.NoError(t, err)
	require.Empty(t, routes[KindOffline])
}

func TestRegistryPluginParticipatesInRoutesAndDelivery(t *testing.T) {
	channel := &testChannel{}
	require.NoError(t, RegisterNotificationChannel("routed-plugin", models.Configuration{}, channel))
	t.Cleanup(func() { _ = UnregisterNotificationChannel("routed-plugin") })
	routes, err := NormalizeRoutes(map[string][]string{KindOffline: {"routed-plugin", "email", "routed-plugin"}})
	require.NoError(t, err)
	require.Equal(t, []string{"routed-plugin", "email"}, routes[KindOffline])
	require.NoError(t, deliverTo("routed-plugin", models.EventMessage{Kind: KindOffline, Event: "Offline"}, "{{event}}"))
	require.Equal(t, 1, channel.sends)
}

func TestDigestWindowValidationAndFlushRechecksPolicy(t *testing.T) {
	for _, raw := range []any{float64(0), float64(3601), 1.5, "5", nil, math.NaN(), math.Inf(1)} {
		_, err := NormalizeDigestSeconds(raw)
		require.Error(t, err)
	}
	for _, raw := range []float64{1, 5, 3600} {
		value, err := NormalizeDigestSeconds(raw)
		require.NoError(t, err)
		require.Equal(t, int(raw), value)
	}
	sender := &recordingSender{}
	queued, _ := newDigestQueue()
	restore := installDigestTest(t, sender, true, queued)
	defer restore()
	require.NoError(t, SendEvent(models.EventMessage{Kind: KindOffline, Event: "Offline", Clients: []models.Client{{UUID: "a"}}}))
	prior := loadNotificationSettings
	loadNotificationSettings = func(map[string]any) (map[string]any, error) {
		return map[string]any{config.NotificationEnabledKey: false}, nil
	}
	queued.snapshot()[0]()
	require.Empty(t, sender.events)
	loadNotificationSettings = prior
	require.NoError(t, SendEvent(models.EventMessage{Kind: KindOffline, Event: "Offline", Clients: []models.Client{{UUID: "b"}}}))
	channelsForKind = func(string) ([]string, error) { return nil, nil }
	queued.snapshot()[1]()
	require.Empty(t, sender.events)
	// Exact wall-clock waiting is delegated to timer tests on Actions, never on the VPS.
	enabled, window := digestFrom(map[string]any{config.NotificationDigestEnabledKey: true, config.NotificationDigestSecondsKey: float64(5)})
	require.True(t, enabled)
	require.Equal(t, 5*time.Second, window)
}
