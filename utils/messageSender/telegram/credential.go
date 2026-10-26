package telegram

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/komari-monitor/komari/utils/messageSender/factory"
	"github.com/komari-monitor/komari/utils/messageSender/outboundhttp"
)

var credentialBotToken = regexp.MustCompile(`^[0-9]+:[A-Za-z0-9_-]+$`)

func (*TelegramSender) SupportsCredentialKind(kind string) bool { return kind == "telegram" }

func (t *TelegramSender) SendCredential(ctx context.Context, d factory.CredentialDelivery) error {
	return t.sendCredential(ctx, d, outboundhttp.NewClient(20*time.Second))
}

func (t *TelegramSender) sendCredential(ctx context.Context, d factory.CredentialDelivery, transport *http.Client) error {
	id, err := factory.NormalizeCredentialAddress(d.Kind, d.Address)
	if err != nil || d.Kind != "telegram" || !credentialBotToken.MatchString(t.BotToken) {
		return factory.ErrCredentialDelivery
	}
	endpoint := t.Endpoint
	if endpoint == "" {
		endpoint = "https://api.telegram.org/bot"
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return factory.ErrCredentialDelivery
	}
	data := url.Values{"chat_id": {id}, "text": {d.Title + "\n\n" + d.Message}, "protect_content": {"true"}}
	// Never inherit parse_mode, alert chat/group IDs or message_thread_id.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+t.BotToken+"/sendMessage", strings.NewReader(data.Encode()))
	if err != nil {
		return factory.ErrCredentialDelivery
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := *transport
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return factory.ErrCredentialDelivery
	}
	defer resp.Body.Close()
	var result struct {
		OK bool `json:"ok"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&result) != nil || !result.OK {
		return factory.ErrCredentialDelivery
	}
	return nil
}
