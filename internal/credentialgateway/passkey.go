package credentialgateway

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/komari-monitor/komari/database/accounts"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/internal/notificationotp"
	"github.com/komari-monitor/komari/internal/passkeycredential"
	credential "github.com/komari-monitor/komari/internal/trustedcredential"
)

func (g *Gateway) BeginPasskeyRegistration(ctx context.Context, name, token, password string) (any, string, error) {
	b, user, err := identity(ctx)
	if err != nil {
		return nil, "", err
	}
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > 80 || len(password) > 1024 {
		return nil, "", credential.ErrInvalidArgument
	}
	if err = g.authorizeEnrollment(ctx, b, user, "passkey:register:"+name, token, password); err != nil {
		return nil, "", err
	}
	return passkeycredential.BeginRegistration(ctx, b, name)
}
func (g *Gateway) CompletePasskeyRegistration(ctx context.Context, id string, raw []byte) (models.UserPasskey, error) {
	b, user, err := identity(ctx)
	if err != nil {
		return models.UserPasskey{}, err
	}
	return passkeycredential.CompleteRegistration(ctx, b, user, id, raw)
}
func (g *Gateway) ListPasskeys(ctx context.Context, token string) ([]models.UserPasskey, error) {
	b, _, err := identity(ctx)
	if err != nil {
		return nil, err
	}
	if _, err = g.core.Consume(ctx, b, token, credential.ManageCredentials, "passkey:list"); err != nil {
		return nil, err
	}
	return passkeycredential.List(ctx, b.UserID)
}
func (g *Gateway) DeletePasskey(ctx context.Context, id, token string) error {
	b, user, err := identity(ctx)
	if err != nil {
		return err
	}
	if _, err = g.core.Consume(ctx, b, token, credential.ManageCredentials, "passkey:delete:"+id); err != nil {
		return err
	}
	contacts, err := accounts.ListCredentialContacts(ctx, user.UUID)
	if err != nil {
		return err
	}
	hasContact := false
	for _, contact := range contacts {
		if !contact.VerifiedAt.IsZero() && validContactTransport(notificationotp.Destination{Kind: contact.Kind, Transport: contact.Transport}) {
			hasContact = true
			break
		}
	}
	return passkeycredential.Delete(ctx, user, id, hasContact)
}
