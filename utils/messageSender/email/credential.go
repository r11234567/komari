package email

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"time"

	"github.com/komari-monitor/komari/utils/messageSender/factory"
)

func (*EmailSender) SupportsCredentialKind(kind string) bool { return kind == "email" }

// SMTP credential messages always require authenticated TLS, even when the
// existing alert transport permits plaintext. The alert path remains unchanged.
func (e *EmailSender) SendCredential(ctx context.Context, d factory.CredentialDelivery) error {
	to, err := factory.NormalizeCredentialAddress(d.Kind, d.Address)
	if err != nil || d.Kind != "email" || strings.ContainsAny(d.Title, "\r\n") {
		return factory.ErrCredentialDelivery
	}
	cfg := e.Addition
	from, err := mail.ParseAddress(cfg.Sender)
	if err != nil || strings.ContainsAny(cfg.Sender+cfg.Host, "\r\n\x00") || cfg.Host == "" || cfg.Port < 1 || cfg.Port > 65535 || cfg.Username == "" || cfg.Password == "" {
		return factory.ErrCredentialDelivery
	}
	message, err := credentialEmailMessage(from, to, d.Title, d.Message)
	if err != nil {
		return factory.ErrCredentialDelivery
	}
	bounded, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	dialer := &net.Dialer{Timeout: 20 * time.Second}
	raw, err := dialer.DialContext(bounded, "tcp", net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)))
	if err != nil {
		return factory.ErrCredentialDelivery
	}
	defer raw.Close()
	stop := context.AfterFunc(bounded, func() { _ = raw.Close() })
	defer stop()
	if deadline, ok := bounded.Deadline(); ok {
		_ = raw.SetDeadline(deadline)
	}
	tlsConfig := &tls.Config{ServerName: cfg.Host, MinVersion: tls.VersionTLS12}
	conn := raw
	if cfg.Port == 465 {
		secure := tls.Client(raw, tlsConfig)
		if err := secure.HandshakeContext(bounded); err != nil {
			return factory.ErrCredentialDelivery
		}
		conn = secure
	}
	client, err := smtp.NewClient(conn, cfg.Host)
	if err != nil {
		return factory.ErrCredentialDelivery
	}
	defer client.Close()
	if cfg.Port != 465 {
		if err := client.StartTLS(tlsConfig); err != nil {
			return factory.ErrCredentialDelivery
		}
	}
	var auth smtp.Auth = smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.Host)
	if cfg.UseLoginAuth {
		auth = &loginAuth{username: cfg.Username, password: cfg.Password, host: cfg.Host}
	}
	if client.Auth(auth) != nil || client.Mail(from.Address) != nil || client.Rcpt(to) != nil {
		return factory.ErrCredentialDelivery
	}
	w, err := client.Data()
	if err != nil {
		return factory.ErrCredentialDelivery
	}
	if _, err := w.Write(message); err != nil {
		_ = w.Close()
		return factory.ErrCredentialDelivery
	}
	if w.Close() != nil {
		return factory.ErrCredentialDelivery
	}
	if client.Quit() != nil {
		return factory.ErrCredentialDelivery
	}
	return bounded.Err()
}

func credentialEmailMessage(from *mail.Address, to, title, body string) ([]byte, error) {
	var encoded bytes.Buffer
	w := quotedprintable.NewWriter(&encoded)
	if _, err := w.Write([]byte(body)); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	headers := []string{"From: " + from.String(), "To: " + to, "Subject: " + mime.QEncoding.Encode("UTF-8", title), "MIME-Version: 1.0", "Content-Type: text/plain; charset=UTF-8", "Content-Transfer-Encoding: quoted-printable", "Date: " + time.Now().UTC().Format(time.RFC1123Z)}
	return []byte(fmt.Sprintf("%s\r\n\r\n%s", strings.Join(headers, "\r\n"), encoded.String())), nil
}
