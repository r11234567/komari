package messageSender

import (
	"encoding/json"
	"fmt"

	"github.com/komari-monitor/komari/utils/messageSender/factory"
)

func LoadProvider(name string, addition string) error {
	providerDeliveryMu.Lock()
	defer providerDeliveryMu.Unlock()
	mu.Lock()
	defer mu.Unlock()
	return loadProviderFromAdditionLocked(name, addition)
}

// UnloadProvider drops a channel that no alert is using, so a saved
// connection is not initialized until a route selects it.
func UnloadProvider(name string) {
	providerDeliveryMu.Lock()
	defer providerDeliveryMu.Unlock()
	mu.Lock()
	defer mu.Unlock()
	provider := providers[name]
	if provider != nil {
		_ = provider.Destroy()
	}
	delete(providers, name)
}

func loadProviderFromAdditionLocked(name, addition string) error {
	constructor, exists := factory.GetConstructor(name)
	if !exists {
		return fmt.Errorf("message sender provider not found: %s", name)
	}
	provider := constructor()
	if err := json.Unmarshal([]byte(addition), provider.GetConfiguration()); err != nil {
		return fmt.Errorf("failed to load config for provider %s: %w", name, err)
	}
	return loadProviderLocked(name, provider)
}

func loadProviderLocked(name string, provider factory.IMessageSender) error {
	if err := provider.Init(); err != nil {
		return fmt.Errorf("failed to init provider %s: %w", name, err)
	}
	if current, ok := providers[name]; ok && current != nil {
		_ = current.Destroy()
	}
	providers[name] = provider
	return nil
}

// 将配置转换为map

func GetProviderConfiguration(name string) (map[string]interface{}, error) {
	constructor, exists := factory.GetConstructor(name)
	if !exists {
		return nil, fmt.Errorf("message sender provider not found: %s", name)
	}

	provider := constructor()
	config := provider.GetConfiguration()

	// 将配置转换为map
	configBytes, err := json.Marshal(config)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal configuration: %w", err)
	}

	var configMap map[string]interface{}
	if err := json.Unmarshal(configBytes, &configMap); err != nil {
		return nil, fmt.Errorf("failed to unmarshal configuration: %w", err)
	}

	return configMap, nil
}
