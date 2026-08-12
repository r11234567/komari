package email

import (
	"strings"
	"testing"
)

func TestSendTextMessageRejectsHeaderInjectionBeforeDial(t *testing.T) {
	base := Addition{
		Host:     "smtp.example.test",
		Port:     25,
		Sender:   "sender@example.test",
		Receiver: "receiver@example.test",
		Username: "sender@example.test",
		Password: "secret",
	}
	tests := []struct {
		name   string
		config Addition
		title  string
	}{
		{name: "subject", config: base, title: "notice\r\nBcc: victim@example.test"},
		{name: "sender", config: func() Addition { value := base; value.Sender += "\r\nBcc: victim@example.test"; return value }()},
		{name: "recipient", config: func() Addition { value := base; value.Receiver += "\r\nBcc: victim@example.test"; return value }()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := (&EmailSender{Addition: test.config}).SendTextMessage("body", test.title)
			if err == nil || !strings.Contains(err.Error(), "line break") && !strings.Contains(err.Error(), "invalid sender") {
				t.Fatalf("header injection error=%v", err)
			}
		})
	}
}
