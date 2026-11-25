package telegram

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/komari-monitor/komari/utils/messageSender/factory"
	"github.com/komari-monitor/komari/utils/messageSender/outboundhttp"
)

type TelegramSender struct {
	Addition
}

func (t *TelegramSender) GetName() string {
	return "telegram"
}

func (t *TelegramSender) GetConfiguration() factory.Configuration {
	return &t.Addition
}

func (t *TelegramSender) Init() error {
	// 初始化逻辑，如果需要的话
	return nil
}

func (t *TelegramSender) Destroy() error {
	// 清理逻辑，如果需要的话
	return nil
}

func (t *TelegramSender) SendTextMessage(message, title string) error {
	fullMessage := message
	if title != "" {
		fullMessage = fmt.Sprintf("<b>%s</b>\n%s", title, message)
	}

	if fullMessage == "" {
		return errors.New("message is empty")
	}

	endpoint := t.Addition.Endpoint + t.Addition.BotToken + "/sendMessage"

	data := url.Values{}
	data.Set("chat_id", t.Addition.ChatID)
	data.Set("text", fullMessage)
	data.Set("parse_mode", "HTML")

	// Add message_thread_id if provided
	if t.Addition.MessageThreadID != "" {
		data.Set("message_thread_id", t.Addition.MessageThreadID)
	}

	client, err := outboundhttp.NewWebhookClient(30 * time.Second)
	if err != nil {
		return err
	}
	resp, err := client.PostForm(endpoint, data)
	if err != nil {
		var requestError *url.Error
		if errors.As(err, &requestError) {
			err = requestError.Err
		}
		return fmt.Errorf("failed to send Telegram message: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		Ok          bool   `json:"ok"`
		Description string `json:"description"`
	}
	body, err := outboundhttp.ReadNotificationResponse(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read Telegram response: %w", err)
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return fmt.Errorf("failed to decode response: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		if result.Description != "" {
			return fmt.Errorf("telegram API error: %s", result.Description)
		}
		return fmt.Errorf("telegram API returned non-OK status: %d", resp.StatusCode)
	}

	if !result.Ok {
		return fmt.Errorf("telegram API error: %s", result.Description)
	}

	return nil
}

func init() {
	factory.RegisterMessageSender(func() factory.IMessageSender {
		return &TelegramSender{}
	})
}

// 确保实现了 IMessageSender 接口
var _ factory.IMessageSender = (*TelegramSender)(nil)
