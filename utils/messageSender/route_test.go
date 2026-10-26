package messageSender

import (
	"errors"
	"strings"
	"testing"

	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/pkg/config"
	"github.com/komari-monitor/komari/utils/messageSender/factory"
)

func TestSeedRoutesCopiesTheActiveChannel(t *testing.T) {
	seeded := SeedRoutes("telegram")
	if len(seeded) != len(Kinds) {
		t.Fatalf("seeded %d kinds, want %d", len(seeded), len(Kinds))
	}
	for _, kind := range Kinds {
		got := seeded[kind]
		if len(got) != 1 || got[0] != "telegram" {
			t.Fatalf("%s = %#v, want [telegram]", kind, got)
		}
	}
	for _, method := range []string{"", "none", "empty"} {
		for _, channels := range SeedRoutes(method) {
			if len(channels) != 0 {
				t.Fatalf("method %q seeded %#v", method, channels)
			}
		}
	}
}

func TestMigrationNeeded(t *testing.T) {
	if !migrationNeeded(true, false, nil) {
		t.Fatal("missing routes should migrate")
	}
	if migrationNeeded(false, false, SeedRoutes("telegram")) {
		t.Fatal("routes that already name a channel should stay")
	}
	if !migrationNeeded(false, false, SeedRoutes("")) {
		t.Fatal("empty routes that were never migrated should copy the active channel")
	}
	if migrationNeeded(false, true, SeedRoutes("")) {
		t.Fatal("an intentional empty table should stay empty")
	}
}

func TestMethodToSeedSkipsMissingConnections(t *testing.T) {
	prev := providerConfigured
	t.Cleanup(func() { providerConfigured = prev })

	providerConfigured = func(name string) (bool, error) {
		return name == "telegram", nil
	}
	got, err := methodToSeed("telegram")
	if err != nil || got != "telegram" {
		t.Fatalf("configured channel = %q, %v", got, err)
	}
	got, err = methodToSeed("email")
	if err != nil || got != "" {
		t.Fatalf("missing connection = %q, %v", got, err)
	}
	got, err = methodToSeed("not-a-provider")
	if err != nil || got != "" {
		t.Fatalf("unknown channel = %q, %v", got, err)
	}
	for _, method := range []string{"", "none", "empty"} {
		got, err = methodToSeed(method)
		if err != nil || got != "" {
			t.Fatalf("method %q seeded %q, %v", method, got, err)
		}
	}
}

func TestNormalizeRoutes(t *testing.T) {
	routes, err := NormalizeRoutes(map[string]any{
		KindPingLatency: []any{"email", "telegram", "email"},
		KindOffline:     []any{"telegram"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(routes[KindPingLatency], ",") != "email,telegram" {
		t.Fatalf("latency routes = %#v", routes[KindPingLatency])
	}
	if strings.Join(routes[KindOffline], ",") != "telegram" {
		t.Fatalf("offline routes = %#v", routes[KindOffline])
	}
	if len(routes[KindOnline]) != 0 {
		t.Fatalf("missing kind was not stored empty: %#v", routes[KindOnline])
	}
	if _, err := NormalizeRoutes(map[string]any{"nope": []any{"email"}}); err == nil {
		t.Fatal("expected unknown kind to fail")
	}
	if _, err := NormalizeRoutes(map[string]any{KindOffline: []any{"missing-channel"}}); err == nil {
		t.Fatal("expected unknown channel to fail")
	}
	if _, err := NormalizeRoutes(map[string]any{KindOffline: []any{"empty"}}); err == nil {
		t.Fatal("expected empty channel to fail")
	}
}

type countingSender struct {
	events int
	texts  int
	err    error
}

func (c *countingSender) GetName() string                         { return "counting" }
func (c *countingSender) GetConfiguration() factory.Configuration { return &struct{}{} }
func (c *countingSender) Init() error                             { return nil }
func (c *countingSender) Destroy() error                          { return nil }
func (c *countingSender) SendTextMessage(string, string) error {
	c.texts++
	if c.err != nil {
		return c.err
	}
	return nil
}
func (c *countingSender) SendEvent(models.EventMessage) error {
	c.events++
	if c.err != nil {
		return c.err
	}
	return nil
}

// textSender is a Telegram-style channel: it has no structured SendEvent.
type textSender struct {
	texts int
	last  string
	title string
	err   error
}

func (s *textSender) GetName() string                         { return "text" }
func (s *textSender) GetConfiguration() factory.Configuration { return &struct{}{} }
func (s *textSender) Init() error                             { return nil }
func (s *textSender) Destroy() error                          { return nil }
func (s *textSender) SendTextMessage(message, title string) error {
	s.texts++
	s.last = message
	s.title = title
	return s.err
}

func TestSendEventFansOutSelectedChannels(t *testing.T) {
	prevLoad := loadNotificationSettings
	prevAudit := writeSendAudit
	prevRoutes := channelsForKind
	mu.Lock()
	prevProviders := providers
	providers = map[string]factory.IMessageSender{}
	mu.Unlock()
	t.Cleanup(func() {
		loadNotificationSettings = prevLoad
		writeSendAudit = prevAudit
		channelsForKind = prevRoutes
		mu.Lock()
		providers = prevProviders
		mu.Unlock()
	})
	writeSendAudit = func(string, string, string, string, map[string]string) {}
	loadNotificationSettings = func(map[string]any) (map[string]any, error) {
		return map[string]any{
			config.NotificationEnabledKey:  true,
			config.NotificationTemplateKey: "Message: {{message}}",
		}, nil
	}
	okSender := &countingSender{}
	failSender := &countingSender{err: errors.New("smtp down")}
	mu.Lock()
	providers = map[string]factory.IMessageSender{
		"ok":   okSender,
		"fail": failSender,
	}
	mu.Unlock()
	channelsForKind = func(kind string) ([]string, error) {
		if kind == KindPingLatency {
			return []string{"ok", "fail"}, nil
		}
		return nil, nil
	}

	err := SendEvent(models.EventMessage{Kind: KindPingLatency, Event: "延迟监测告警 · 延迟异常", Message: "late"})
	if err != nil {
		t.Fatal(err)
	}
	if okSender.events != 1 {
		t.Fatalf("successful channel events = %d, want 1", okSender.events)
	}
	if failSender.events != 3 {
		t.Fatalf("failed channel attempts = %d, want 3", failSender.events)
	}

	if err := SendEvent(models.EventMessage{Kind: KindOffline, Event: "Offline"}); err != nil {
		t.Fatal(err)
	}
	if okSender.events != 1 {
		t.Fatal("empty route sent a notification")
	}
	if err := SendEvent(models.EventMessage{Event: "Offline"}); err == nil {
		t.Fatal("expected missing kind to fail")
	}
}

func TestSendEventReachesLaterChannelsWhenAnEarlierOneFails(t *testing.T) {
	prevLoad := loadNotificationSettings
	prevAudit := writeSendAudit
	prevRoutes := channelsForKind
	mu.Lock()
	prevProviders := providers
	mu.Unlock()
	t.Cleanup(func() {
		loadNotificationSettings = prevLoad
		writeSendAudit = prevAudit
		channelsForKind = prevRoutes
		mu.Lock()
		providers = prevProviders
		mu.Unlock()
	})
	writeSendAudit = func(string, string, string, string, map[string]string) {}
	loadNotificationSettings = func(map[string]any) (map[string]any, error) {
		return map[string]any{
			config.NotificationEnabledKey:       true,
			config.NotificationTemplateKey:      "Clients: {{client}}\nMessage: {{message}}",
			config.NotificationDigestEnabledKey: false,
		}, nil
	}
	failed := &countingSender{err: errors.New("telegram down")}
	structured := &countingSender{}
	plain := &textSender{}
	mu.Lock()
	providers = map[string]factory.IMessageSender{
		"failed":     failed,
		"structured": structured,
		"plain":      plain,
	}
	mu.Unlock()
	channelsForKind = func(kind string) ([]string, error) {
		if kind == KindLoad {
			return []string{"failed", "structured", "plain"}, nil
		}
		return nil, nil
	}

	first := models.EventMessage{
		Kind: KindLoad, Event: "Alert", Message: "CPU",
		Clients: []models.Client{{UUID: "a", Name: "alpha"}},
	}
	second := models.EventMessage{
		Kind: KindLoad, Event: "Alert", Message: "CPU",
		Clients: []models.Client{{UUID: "b", Name: "beta"}},
	}
	if err := SendEvent(first); err != nil {
		t.Fatal(err)
	}
	if err := SendEvent(second); err != nil {
		t.Fatal(err)
	}
	if failed.events != 6 {
		t.Fatalf("failed channel attempts = %d, want 6", failed.events)
	}
	if structured.events != 2 {
		t.Fatalf("second channel events = %d, want 2", structured.events)
	}
	if plain.texts != 2 || plain.title != "Alert" || plain.last != "Clients: beta\nMessage: CPU" {
		t.Fatalf("text channel texts=%d title=%q body=%q", plain.texts, plain.title, plain.last)
	}

	failed.err = errors.New("still down")
	structured.err = errors.New("script down")
	plain.err = errors.New("webhook down")
	err := SendEvent(first)
	if err == nil || !strings.Contains(err.Error(), "webhook down") {
		t.Fatalf("all channels failed, err = %v", err)
	}
	if plain.texts != 5 {
		t.Fatalf("text channel was not retried after the others failed: %d", plain.texts)
	}
}

func TestSendTestEventTargetsOneChannel(t *testing.T) {
	prevLoad := loadNotificationSettings
	prevAudit := writeSendAudit
	prevRoutes := channelsForKind
	mu.Lock()
	prevProviders := providers
	mu.Unlock()
	t.Cleanup(func() {
		loadNotificationSettings = prevLoad
		writeSendAudit = prevAudit
		channelsForKind = prevRoutes
		mu.Lock()
		providers = prevProviders
		mu.Unlock()
	})
	writeSendAudit = func(string, string, string, string, map[string]string) {}
	loadNotificationSettings = func(map[string]any) (map[string]any, error) {
		return map[string]any{
			config.NotificationEnabledKey:  false,
			config.NotificationTemplateKey: "Message: {{message}}",
		}, nil
	}
	sender := &countingSender{}
	mu.Lock()
	providers = map[string]factory.IMessageSender{"email": sender}
	mu.Unlock()
	channelsForKind = func(string) ([]string, error) {
		t.Fatal("test send consulted the route table")
		return nil, nil
	}

	if err := SendEvent(models.EventMessage{Kind: KindOffline, Event: "Offline", Message: "down"}); err != nil {
		t.Fatal(err)
	}
	if sender.events != 0 {
		t.Fatalf("disabled switch sent a real event: %d", sender.events)
	}
	if err := SendTestEventTo("email", models.EventMessage{Event: "Test", Message: "hello"}); err != nil {
		t.Fatal(err)
	}
	if sender.events != 1 {
		t.Fatalf("test message was not sent, events=%d", sender.events)
	}
}
