package email

import (
	"bytes"
	"context"
	"io"
	"mime/quotedprintable"
	"net/mail"
	"strings"
	"testing"

	"github.com/komari-monitor/komari/utils/messageSender/factory"
)

func TestCredentialEmailHasOnlyExplicitRecipientAndPlainText(t *testing.T) {
	from, _ := mail.ParseAddress("Komari <sender@example.test>")
	raw, err := credentialEmailMessage(from, "private@example.test", "Synthetic verification", "Synthetic code: 123456\nDo not share.")
	if err != nil {
		t.Fatal(err)
	}
	m, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if m.Header.Get("To") != "private@example.test" || m.Header.Get("Bcc") != "" || m.Header.Get("Cc") != "" || m.Header.Get("Content-Type") != "text/plain; charset=UTF-8" {
		t.Fatal("unexpected recipient/header")
	}
	body, err := io.ReadAll(quotedprintable.NewReader(m.Body))
	if err != nil || !strings.Contains(string(body), "123456") {
		t.Fatal("invalid MIME body")
	}
}

func TestCredentialSMTPHonorsCancelledContextWithoutSending(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sender := &EmailSender{Addition: Addition{Host: "smtp.example.test", Port: 587, Sender: "sender@example.test", Username: "synthetic", Password: "synthetic", Receiver: "alerts@example.test"}}
	if err := sender.SendCredential(ctx, factory.CredentialDelivery{Kind: "email", Address: "private@example.test", Title: "Synthetic", Message: "Synthetic"}); err == nil {
		t.Fatal("cancelled operation accepted")
	}
	if sender.Receiver != "alerts@example.test" {
		t.Fatal("alert recipient was changed")
	}
}
