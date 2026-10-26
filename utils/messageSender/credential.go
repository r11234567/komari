package messageSender

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/komari-monitor/komari/database"
	"github.com/komari-monitor/komari/utils/messageSender/factory"
)

type CredentialTransport struct {
	ID    string   `json:"id"`
	Kinds []string `json:"kinds"`
}

// ListCredentialTransports returns metadata only, never channel configuration.
// Only explicitly addressed built-in transports are eligible at this stage.
// Legacy JS sendMessage/sendEvent scripts have no private-recipient contract.
func ListCredentialTransports() []CredentialTransport {
	result := []CredentialTransport{}
	for _, id := range factory.GetAllMessageSenderNames() {
		constructor, _ := factory.GetConstructor(id)
		sender, ok := constructor().(factory.CredentialSender)
		if !ok {
			continue
		}
		if _, err := database.GetMessageSenderConfigByName(id); err != nil {
			continue
		}
		item := CredentialTransport{ID: id}
		for _, kind := range []string{"email", "telegram"} {
			if sender.SupportsCredentialKind(kind) {
				item.Kinds = append(item.Kinds, kind)
			}
		}
		if len(item.Kinds) > 0 {
			result = append(result, item)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

// SendCredential reuses saved transport credentials using a new private sender
// instance. It never changes live alert recipients or enters routing/digest/
// templates. It is Go-only, not a general-purpose send-to-anyone RPC method.
func SendCredential(ctx context.Context, id string, delivery factory.CredentialDelivery) error {
	if ctx == nil {
		return factory.ErrCredentialDelivery
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	address, err := factory.NormalizeCredentialAddress(delivery.Kind, delivery.Address)
	if err != nil || delivery.Title == "" || len(delivery.Title) > 256 || strings.ContainsAny(delivery.Title, "\r\n") || len(delivery.Message) == 0 || len(delivery.Message) > 8192 {
		return factory.ErrCredentialDelivery
	}
	delivery.Address = address
	constructor, ok := factory.GetConstructor(id)
	if !ok {
		return factory.ErrCredentialDelivery
	}
	provider := constructor()
	sender, ok := provider.(factory.CredentialSender)
	if !ok || !sender.SupportsCredentialKind(delivery.Kind) {
		return factory.ErrCredentialDelivery
	}
	config, err := database.GetMessageSenderConfigByName(id)
	if err != nil || json.Unmarshal([]byte(config.Addition), provider.GetConfiguration()) != nil {
		return factory.ErrCredentialDelivery
	}
	bounded, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := sender.SendCredential(bounded, delivery); err != nil {
		if bounded.Err() != nil {
			return bounded.Err()
		}
		// Transport errors can contain SMTP recipients, Telegram tokens, URLs or
		// remote response bodies. Never propagate them into auth errors/logs.
		return factory.ErrCredentialDelivery
	}
	return bounded.Err()
}
