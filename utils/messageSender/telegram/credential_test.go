package telegram

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/komari-monitor/komari/utils/messageSender/factory"
)

func TestCredentialTelegramUsesOnlyPrivateID(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.ParseForm() != nil {
			t.Error("invalid credential request")
		}
		if r.Form.Get("chat_id") != "123456789" || r.Form.Get("message_thread_id") != "" || r.Form.Get("parse_mode") != "" || r.Form.Get("protect_content") != "true" {
			t.Error("alert metadata inherited")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()
	sender := &TelegramSender{Addition: Addition{Endpoint: server.URL + "/bot", BotToken: "123:synthetic-token", ChatID: "-100123456", MessageThreadID: "123"}}
	err := sender.sendCredential(context.Background(), factory.CredentialDelivery{Kind: "telegram", Address: "123456789", Title: "Synthetic", Message: "Synthetic code"}, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if sender.ChatID != "-100123456" || sender.MessageThreadID != "123" {
		t.Fatal("alert settings mutated")
	}
}
