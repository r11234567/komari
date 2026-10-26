package messageSender

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/komari-monitor/komari/database/dbcore"
	"strings"
	"sync"
	"time"

	logger "github.com/komari-monitor/komari/utils/log"

	"github.com/komari-monitor/komari/database"
	"github.com/komari-monitor/komari/database/auditlog"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/pkg/config"
	"github.com/komari-monitor/komari/utils/messageSender/factory"
	"gorm.io/gorm"
)

var (
	providers                = map[string]factory.IMessageSender{}
	mu                       = sync.Mutex{}
	providerDeliveryMu       = sync.RWMutex{}
	once                     = sync.Once{}
	loadNotificationSettings = config.GetMany
	writeSendAudit           = func(ip, actor, level, event string, fields map[string]string) {
		auditlog.Log(ip, actor, fmt.Sprintf("%s %v", event, fields), level)
	}
	channelsForKind = storedChannels
)

func CurrentProvider() factory.IMessageSender {
	mu.Lock()
	defer mu.Unlock()
	if provider, ok := providers["email"]; ok {
		return provider
	}
	for _, provider := range providers {
		return provider
	}
	return nil
}

func providerByName(name string) factory.IMessageSender {
	mu.Lock()
	defer mu.Unlock()
	return providers[name]
}

// Shutdown 销毁已加载的消息发送 provider。供关闭流程调用。
func Shutdown() error {
	flushDigestBatches()
	providerDeliveryMu.Lock()
	defer providerDeliveryMu.Unlock()
	mu.Lock()
	defer mu.Unlock()
	var errs []error
	for name, provider := range providers {
		if provider != nil {
			if err := provider.Destroy(); err != nil {
				errs = append(errs, err)
			}
		}
		delete(providers, name)
	}
	return errors.Join(errs...)
}

func Initialize() {
	if err := EnsureRoutes(); err != nil {
		logger.Errorf("message-sender", "Failed to migrate routes: %v", err)
		return
	}

	go func() {
		once.Do(func() {
			all := factory.GetAllMessageSenders()
			for _, provider := range all {
				if _, err := database.GetMessageSenderConfigByName(provider.GetName()); err == nil {
					continue
				}
				// 如果数据库中没有该提供者的配置，则保存默认配置
				config := provider.GetConfiguration()
				configBytes, err := json.Marshal(config)
				if err != nil {
					logger.Errorf("message-sender", "Failed to marshal config for provider %s: %v", provider.GetName(), err)
					return
				}
				if err := database.SaveMessageSenderConfig(&models.MessageSenderProvider{
					Name:     provider.GetName(),
					Addition: string(configBytes),
				}); err != nil {
					logger.Errorf("message-sender", "Failed to save default config for provider %s: %v", provider.GetName(), err)
					return
				}
			}
		})
	}()
	if err := SyncProviders(); err != nil {
		logger.Errorf("message-sender", "Failed to load notification channels: %v", err)
	}
}

func SendNotification(event models.EventMessage) error {
	if len(event.Clients) > 0 {
		db := dbcore.GetDBInstance()
		eventClients := make([]models.Client, 0, len(event.Clients))
		for _, c := range event.Clients {
			if c.UUID == "" {
				continue
			}
			var full models.Client
			if err := db.Where("uuid = ?", c.UUID).First(&full).Error; err == nil {
				eventClients = append(eventClients, full)
			}
		}
		if len(eventClients) == 0 {
			return fmt.Errorf("none of the specified clients exist")
		}
		event.Clients = eventClients
	}
	if event.Kind == "" {
		event.Kind = KindForEvent(event.Event)
	}
	if event.Kind == "" {
		enabled, err := config.GetAs[bool](config.NotificationEnabledKey, false)
		if err != nil || !enabled {
			return err
		}
		name, err := config.GetAs[string](config.NotificationMethodKey, "none")
		if err != nil || name == "" || name == "none" {
			return err
		}
		_, template, err := notificationDeliverySettings()
		if err != nil {
			return err
		}
		if event.Time.IsZero() {
			event.Time = time.Now().UTC()
		}
		return deliverTo(name, event, template)
	}
	return SendEvent(event)
}

// SendTextMessage retains the legacy direct-send API used by integrations.
func SendTextMessage(message, title string) error {
	enabled, err := config.GetAs[bool](config.NotificationEnabledKey, false)
	if err != nil || !enabled {
		return err
	}
	method, err := config.GetAs[string](config.NotificationMethodKey, "none")
	if err != nil || method == "none" || method == "" {
		return err
	}
	return deliverTo(method, models.EventMessage{Event: title, Message: message, Time: time.Now().UTC()}, "{{message}}")
}

func SendEvent(event models.EventMessage) error {
	if event.Time.IsZero() {
		event.Time = time.Now().UTC()
	} else {
		event.Time = event.Time.UTC()
	}
	cfg, template, err := notificationDeliverySettings()
	if err != nil {
		return err
	}
	enabled, _ := cfg[config.NotificationEnabledKey].(bool)
	if !enabled {
		return nil
	}
	if strings.TrimSpace(event.Kind) == "" {
		return fmt.Errorf("notification kind is required")
	}
	channels, err := channelsForKind(event.Kind)
	if err != nil {
		return err
	}
	if len(channels) == 0 {
		return nil
	}
	if digestOn, window := digestFrom(cfg); digestOn && digestibleKind(event.Kind) {
		// Return before the window closes. A caller that sends servers one
		// after another, such as the load alert loop, must join the same batch.
		enqueueDigest(event, window)
		return nil
	}
	return deliverPrepared(event, channels, template)
}

func deliverEvent(event models.EventMessage) error {
	if event.Time.IsZero() {
		event.Time = time.Now().UTC()
	} else {
		event.Time = event.Time.UTC()
	}
	cfg, template, err := notificationDeliverySettings()
	if err != nil {
		return err
	}
	enabled, _ := cfg[config.NotificationEnabledKey].(bool)
	if !enabled {
		return nil
	}
	if strings.TrimSpace(event.Kind) == "" {
		return fmt.Errorf("notification kind is required")
	}
	channels, err := channelsForKind(event.Kind)
	if err != nil {
		return err
	}
	if len(channels) == 0 {
		return nil
	}
	return deliverPrepared(event, channels, template)
}

func deliverPrepared(event models.EventMessage, channels []string, template string) error {
	var failures []error
	sent := 0
	for _, name := range channels {
		if err := deliverTo(name, event, template); err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", name, err))
			writeSendAudit("", "", "error", "audit.event_fail", map[string]string{
				"event": event.Event,
				"error": name + ": " + err.Error(),
			})
			logger.Errorf("message-sender", "Failed to send %s via %s: %v", event.Event, name, err)
			continue
		}
		sent++
		writeSendAudit("", "", "info", "audit.event_ok", map[string]string{
			"event":   event.Event,
			"channel": name,
		})
	}
	if sent == 0 {
		return errors.Join(failures...)
	}
	return nil
}

// SendTestEventTo delivers a manual test to one channel.
// The master notification switch and the route table do not apply.
func SendTestEventTo(name string, event models.EventMessage) error {
	name = strings.TrimSpace(name)
	if name == "" || name == "none" || name == "empty" {
		return fmt.Errorf("provider is required")
	}
	if _, ok := factory.GetConstructor(name); !ok && !NotificationChannelRegistered(name) {
		return fmt.Errorf("message sender provider not found: %s", name)
	}
	if event.Time.IsZero() {
		event.Time = time.Now().UTC()
	} else {
		event.Time = event.Time.UTC()
	}
	_, template, err := notificationDeliverySettings()
	if err != nil {
		template = "{{emoji}}{{emoji}}{{emoji}}\nEvent: {{event}}\nClients: {{client}}\nMessage: {{message}}\nTime: {{time}}"
	}
	if err := deliverTo(name, event, template); err != nil {
		writeSendAudit("", "", "error", "audit.event_fail", map[string]string{
			"event": event.Event,
			"error": name + ": " + err.Error(),
		})
		return err
	}
	writeSendAudit("", "", "info", "audit.event_ok", map[string]string{
		"event":   event.Event,
		"channel": name,
	})
	return nil
}

func notificationDeliverySettings() (map[string]any, string, error) {
	cfg, err := loadNotificationSettings(map[string]any{
		config.NotificationEnabledKey:       false,
		config.NotificationTemplateKey:      "{{emoji}}{{emoji}}{{emoji}}\nEvent: {{event}}\nClients: {{client}}\nMessage: {{message}}\nTime: {{time}}",
		config.NotificationDigestEnabledKey: false,
		config.NotificationDigestSecondsKey: DefaultDigestSeconds,
	})
	if err != nil {
		return nil, "", err
	}
	template, _ := cfg[config.NotificationTemplateKey].(string)
	return cfg, template, nil
}

// SyncProviders loads every channel selected by the route table and drops the rest.
func SyncProviders() error {
	providerDeliveryMu.Lock()
	defer providerDeliveryMu.Unlock()
	routes, err := config.GetAs[map[string][]string](config.NotificationRoutesKey)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		return err
	}
	needed := map[string]struct{}{}
	for _, channels := range routes {
		for _, name := range channels {
			if name == "" || NotificationChannelRegistered(name) {
				continue
			}
			needed[name] = struct{}{}
		}
	}

	mu.Lock()
	defer mu.Unlock()
	for name, provider := range providers {
		if _, ok := needed[name]; ok {
			continue
		}
		if provider != nil {
			_ = provider.Destroy()
		}
		delete(providers, name)
	}
	var errs []error
	for name := range needed {
		cfg, lookupErr := database.GetMessageSenderConfigByName(name)
		if lookupErr != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, lookupErr))
			continue
		}
		if err := loadProviderFromAdditionLocked(name, cfg.Addition); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func deliverTo(name string, event models.EventMessage, template string) error {
	if NotificationChannelRegistered(name) {
		return CallNotificationChannel(context.Background(), name, Notification{Event: event, Title: event.Event, Message: parseTemplate(template, event)})
	}

	provider := providerByName(name)
	if provider == nil {
		cfg, err := database.GetMessageSenderConfigByName(name)
		if err != nil {
			return err
		}
		if err := LoadProvider(name, cfg.Addition); err != nil {
			return err
		}
		provider = providerByName(name)
	}
	providerDeliveryMu.RLock()
	defer providerDeliveryMu.RUnlock()
	provider = providerByName(name)
	if provider == nil {
		return fmt.Errorf("message sender %s is not loaded", name)
	}
	var err error
	if eventSender, ok := provider.(factory.IEventMessageSender); ok {
		for range 3 {
			err = eventSender.SendEvent(event)
			if deliverySucceeded(err) {
				return nil
			}
		}
		return err
	}
	message := parseTemplate(template, event)
	for range 3 {
		err = provider.SendTextMessage(message, event.Event)
		if deliverySucceeded(err) {
			return nil
		}
	}
	return err
}

func deliverySucceeded(err error) bool {
	return err == nil || err.Error() == "short response: \x00\x00\x00\x1a\x00\x00\x00"
}

func parseTemplate(messageTemplate string, event models.EventMessage) string {
	// Aggregate client names. If Name is empty, fall back to UUID.
	clientNames := make([]string, 0, len(event.Clients))
	for _, c := range event.Clients {
		name := c.Name
		if strings.TrimSpace(name) == "" {
			// fallback to UUID when name is not set
			name = c.UUID
		}
		clientNames = append(clientNames, name)
	}
	joinedClients := strings.Join(clientNames, ", ")

	replaceMap := map[string]string{
		"{{event}}":   event.Event,
		"{{client}}":  joinedClients,
		"{{time}}":    event.Time.UTC().Format(time.RFC3339Nano),
		"{{message}}": event.Message,
		"{{emoji}}":   event.Emoji,
	}
	result := messageTemplate
	for placeholder, value := range replaceMap {
		result = strings.ReplaceAll(result, placeholder, value)
	}
	return result
}
