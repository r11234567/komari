package jsonrpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/komari-monitor/komari/internal/managedconfig"
	"github.com/komari-monitor/komari/utils/item"
	"gorm.io/gorm"
	"strings"
	"unicode/utf8"

	"github.com/komari-monitor/komari/database"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/pkg/config"
	"github.com/komari-monitor/komari/pkg/rpc"
	"github.com/komari-monitor/komari/utils/messageSender"
	msfactory "github.com/komari-monitor/komari/utils/messageSender/factory"
	"github.com/komari-monitor/komari/web/oauth"
	oauthfactory "github.com/komari-monitor/komari/web/oauth/factory"
)

// admin.provider.go
// 消息发送器与 OIDC 提供者配置 RPC2 方法（admin 命名空间）。

func init() {
	reg("getMessageSenderProvider", adminGetMessageSender, "Get message sender provider config or templates")
	reg("setMessageSenderProvider", adminSetMessageSender, "Set message sender provider config")
	reg("getOidcProvider", adminGetOidc, "Get OIDC provider config or templates")
	reg("setOidcProvider", adminSetOidc, "Set OIDC provider config")
}

const MaskRune = '●' // \u25cf

func isSecretFieldName(name string) bool {
	lower := strings.ToLower(name)
	return lower == "password" || lower == "bot_token" || lower == "device_key" ||
		lower == "secret" || lower == "client_secret"
}

func isMaskedSecret(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r != MaskRune && r != '*' {
			return false
		}
	}
	return true
}

func maskSecretString(s string) string {
	count := utf8.RuneCountInString(s)
	if count == 0 {
		return ""
	}
	return strings.Repeat(string(MaskRune), count)
}

func getSenderSecretFields(providerName string) map[string]bool {
	fields := make(map[string]bool)
	configs := senderConfigurations()
	if items, ok := configs[providerName]; ok {
		for _, it := range items {
			if it.Secret || it.Type == "password" || it.Type == "secret" || isSecretFieldName(it.Name) {
				fields[it.Name] = true
			}
		}
	}
	return fields
}

func getOidcSecretFields(providerName string) map[string]bool {
	fields := make(map[string]bool)
	configs := oauthfactory.GetProviderConfigs()
	if items, ok := configs[providerName]; ok {
		for _, it := range items {
			if it.Secret || it.Type == "password" || it.Type == "secret" || isSecretFieldName(it.Name) {
				fields[it.Name] = true
			}
		}
	}
	return fields
}

func maskAdditionSecrets(_ string, additionJSON string, secretFields map[string]bool) string {
	if additionJSON == "" || additionJSON == "{}" {
		return additionJSON
	}
	var data map[string]any
	if err := json.Unmarshal([]byte(additionJSON), &data); err != nil {
		return additionJSON
	}
	changed := false
	for k, v := range data {
		if secretFields[k] || isSecretFieldName(k) {
			if strVal, ok := v.(string); ok && strVal != "" && !isMaskedSecret(strVal) {
				data[k] = maskSecretString(strVal)
				changed = true
			}
		}
	}
	if !changed {
		return additionJSON
	}
	out, err := json.Marshal(data)
	if err != nil {
		return additionJSON
	}
	return string(out)
}

func restoreAdditionSecrets(newAdditionJSON string, existingAdditionJSON string, secretFields map[string]bool) string {
	if newAdditionJSON == "" || existingAdditionJSON == "" {
		return newAdditionJSON
	}
	var newMap map[string]any
	if err := json.Unmarshal([]byte(newAdditionJSON), &newMap); err != nil {
		return newAdditionJSON
	}
	var oldMap map[string]any
	if err := json.Unmarshal([]byte(existingAdditionJSON), &oldMap); err != nil {
		return newAdditionJSON
	}
	changed := false
	for k, v := range newMap {
		if secretFields[k] || isSecretFieldName(k) {
			if strVal, ok := v.(string); ok && isMaskedSecret(strVal) {
				if oldVal, ok := oldMap[k].(string); ok && oldVal != "" && !isMaskedSecret(oldVal) {
					newMap[k] = oldVal
					changed = true
				}
			}
		}
	}
	if !changed {
		return newAdditionJSON
	}
	out, err := json.Marshal(newMap)
	if err != nil {
		return newAdditionJSON
	}
	return string(out)
}

func adminGetMessageSender(_ context.Context, req *rpc.JsonRpcRequest) (any, *rpc.JsonRpcError) {
	var params struct {
		Provider string `json:"provider"`
	}
	req.BindParams(&params)
	if params.Provider != "" {
		cfg, err := database.GetMessageSenderConfigByName(params.Provider)
		if errors.Is(err, gorm.ErrRecordNotFound) && messageSender.NotificationChannelRegistered(params.Provider) {
			defaults := map[string]any{}
			for _, it := range senderConfigurations()[params.Provider] {
				defaults[it.Name] = it.Default
			}
			raw, _ := json.Marshal(defaults)
			cfg = &models.MessageSenderProvider{Name: params.Provider, Addition: string(raw)}
			err = nil
		}
		if err != nil {
			return nil, rpc.MakeError(rpc.NotFound, "Provider not found: "+err.Error(), nil)
		}
		secretFields := getSenderSecretFields(params.Provider)
		maskedCfg := *cfg
		maskedCfg.Addition = maskAdditionSecrets(params.Provider, cfg.Addition, secretFields)
		return &maskedCfg, nil
	}
	providers := senderConfigurations()
	if len(providers) == 0 {
		return nil, rpc.MakeError(rpc.NotFound, "No message sender providers found", nil)
	}
	return providers, nil
}

func adminSetMessageSender(_ context.Context, req *rpc.JsonRpcRequest) (any, *rpc.JsonRpcError) {
	var senderConfig models.MessageSenderProvider
	if err := req.BindParams(&senderConfig); err != nil {
		return nil, rpc.MakeError(rpc.InvalidParams, "Invalid configuration: "+err.Error(), nil)
	}
	if senderConfig.Name == "" {
		return nil, rpc.MakeError(rpc.InvalidParams, "Provider name is required", nil)
	}
	if _, exists := msfactory.GetConstructor(senderConfig.Name); !exists && !messageSender.NotificationChannelRegistered(senderConfig.Name) {
		return nil, rpc.MakeError(rpc.NotFound, "Provider not found: "+senderConfig.Name, nil)
	}
	secretFields := getSenderSecretFields(senderConfig.Name)
	if existing, err := database.GetMessageSenderConfigByName(senderConfig.Name); err == nil && existing != nil {
		senderConfig.Addition = restoreAdditionSecrets(senderConfig.Addition, existing.Addition, secretFields)
	}
	if err := database.SaveMessageSenderConfig(&senderConfig); err != nil {
		return nil, rpc.MakeError(rpc.InternalError, "Failed to save message sender provider configuration: "+err.Error(), nil)
	}
	selected, err := messageSender.ChannelSelected(senderConfig.Name)
	if err != nil {
		return nil, rpc.MakeError(rpc.InternalError, err.Error(), nil)
	}
	if selected && !messageSender.NotificationChannelRegistered(senderConfig.Name) {
		if err := messageSender.LoadProvider(senderConfig.Name, senderConfig.Addition); err != nil {
			return nil, rpc.MakeError(rpc.InternalError, "Failed to load message sender provider: "+err.Error(), nil)
		}
	}
	return map[string]any{"message": "Message sender provider set successfully"}, nil
}

func adminGetOidc(_ context.Context, req *rpc.JsonRpcRequest) (any, *rpc.JsonRpcError) {
	var params struct {
		Provider string `json:"provider"`
	}
	req.BindParams(&params)
	if params.Provider != "" {
		cfg, err := database.GetOidcConfigByName(params.Provider)
		if err != nil {
			return nil, rpc.MakeError(rpc.NotFound, "Provider not found: "+err.Error(), nil)
		}
		secretFields := getOidcSecretFields(params.Provider)
		maskedCfg := *cfg
		maskedCfg.Addition = maskAdditionSecrets(params.Provider, cfg.Addition, secretFields)
		return &maskedCfg, nil
	}
	providers := oauthfactory.GetProviderConfigs()
	if len(providers) == 0 {
		return nil, rpc.MakeError(rpc.NotFound, "No OIDC providers found", nil)
	}
	return providers, nil
}

func adminSetOidc(_ context.Context, req *rpc.JsonRpcRequest) (any, *rpc.JsonRpcError) {
	var oidcConfig models.OidcProvider
	if err := req.BindParams(&oidcConfig); err != nil {
		return nil, rpc.MakeError(rpc.InvalidParams, "Invalid configuration: "+err.Error(), nil)
	}
	if oidcConfig.Name == "" {
		return nil, rpc.MakeError(rpc.InvalidParams, "Provider name is required", nil)
	}
	if _, exists := oauthfactory.GetConstructor(oidcConfig.Name); !exists {
		return nil, rpc.MakeError(rpc.NotFound, "Provider not found: "+oidcConfig.Name, nil)
	}
	secretFields := getOidcSecretFields(oidcConfig.Name)
	if existing, err := database.GetOidcConfigByName(oidcConfig.Name); err == nil && existing != nil {
		oidcConfig.Addition = restoreAdditionSecrets(oidcConfig.Addition, existing.Addition, secretFields)
	}
	if err := database.SaveOidcConfig(&oidcConfig); err != nil {
		return nil, rpc.MakeError(rpc.InternalError, "Failed to save OIDC provider configuration: "+err.Error(), nil)
	}
	provider, _ := config.GetAs[string](config.OAuthProviderKey, "github")
	if provider == oidcConfig.Name { // 正在使用，重载
		if err := oauth.LoadProvider(oidcConfig.Name, oidcConfig.Addition); err != nil {
			return nil, rpc.MakeError(rpc.InternalError, "Failed to load OIDC provider: "+err.Error(), nil)
		}
	}
	return map[string]any{"message": "OIDC provider set successfully"}, nil
}

func senderConfigurations() map[string][]item.Item {
	out := map[string][]item.Item{}
	for name, fields := range msfactory.GetSenderConfigs() {
		out[name] = fields
	}
	for _, channel := range messageSender.ListNotificationChannels() {
		fields := []item.Item{}
		for _, field := range managedconfig.Items(channel.Configuration) {
			kind := field.Type
			switch kind {
			case "select":
				kind = "option"
			case "switch":
				kind = "bool"
			case "number":
				kind = "float64"
			}
			value := managedconfig.DefaultValue(field)
			def := ""
			if value != nil {
				def = fmt.Sprint(value)
			}
			fields = append(fields, item.Item{Name: field.Key, Required: field.Required, Type: kind, Options: field.Options, Default: def, Help: fmt.Sprint(field.Help), Secret: kind == "password" || kind == "secret" || isSecretFieldName(field.Key)})
		}
		out[channel.ID] = fields
	}
	return out
}
