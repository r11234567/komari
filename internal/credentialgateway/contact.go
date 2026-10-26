package credentialgateway

import (
	"context"
	"errors"

	"github.com/komari-monitor/komari/database/accounts"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/internal/notificationotp"
	"github.com/komari-monitor/komari/internal/passkeycredential"
	credential "github.com/komari-monitor/komari/internal/trustedcredential"
	"github.com/komari-monitor/komari/utils/messageSender"
	"github.com/komari-monitor/komari/utils/messageSender/factory"
)

type ContactSettings struct {
	Contacts                 []models.CredentialContact
	Transports               []messageSender.CredentialTransport
	PasswordBootstrapAllowed bool
}

func (g *Gateway) Contacts(ctx context.Context) (ContactSettings, error) {
	_, user, err := identity(ctx)
	if err != nil {
		return ContactSettings{}, err
	}
	contacts, err := accounts.ListCredentialContacts(ctx, user.UUID)
	if err != nil {
		return ContactSettings{}, credential.ErrProviderUnavailable
	}
	passkey, err := passkeycredential.Read(ctx, user.UUID)
	if err != nil {
		return ContactSettings{}, credential.ErrProviderUnavailable
	}
	return ContactSettings{Contacts: contacts, Transports: messageSender.ListCredentialTransports(), PasswordBootstrapAllowed: user.TwoFactor == "" && len(contacts) == 0 && len(passkey.Records) == 0}, nil
}

func resolveContact(ctx context.Context, b credential.Binding, kind string) (notificationotp.Destination, error) {
	if err := validateBinding(ctx, b); err != nil {
		return notificationotp.Destination{}, err
	}
	contacts, err := accounts.ListCredentialContacts(ctx, b.UserID)
	if err != nil {
		return notificationotp.Destination{}, credential.ErrProviderUnavailable
	}
	for _, contact := range contacts {
		if contact.Kind == kind && !contact.VerifiedAt.IsZero() {
			d := notificationotp.Destination{Kind: kind, Address: contact.Address, Transport: contact.Transport}
			if validContactTransport(d) {
				return d, nil
			}
		}
	}
	return notificationotp.Destination{}, credential.ErrProviderUnavailable
}

func validContactTransport(d notificationotp.Destination) bool {
	for _, transport := range messageSender.ListCredentialTransports() {
		if transport.ID == d.Transport {
			for _, kind := range transport.Kinds {
				if kind == d.Kind {
					return true
				}
			}
		}
	}
	return false
}

func normalizeContact(d notificationotp.Destination) (notificationotp.Destination, error) {
	address, err := factory.NormalizeCredentialAddress(d.Kind, d.Address)
	if err != nil || !validContactTransport(d) {
		return notificationotp.Destination{}, credential.ErrInvalidArgument
	}
	d.Address = address
	return d, nil
}

// ContactResource has a stable client representation: kind and transport are
// server-declared ASCII identifiers; the final suffix is the canonical address.
func ContactResource(d notificationotp.Destination) string {
	return "contact:bind:" + d.Kind + ":" + d.Transport + ":" + d.Address
}
func DeleteContactResource(kind string) string { return "contact:delete:" + kind }

type bindingIntentKey struct{}
type bindingIntent struct {
	binding     credential.Binding
	destination notificationotp.Destination
}

func (g *Gateway) BeginContactBinding(ctx context.Context, d notificationotp.Destination, authorization, password string) (credential.Challenge, error) {
	b, user, err := identity(ctx)
	if err != nil {
		return credential.Challenge{}, err
	}
	d, err = normalizeContact(d)
	if err != nil {
		return credential.Challenge{}, err
	}
	if authorization != "" {
		if _, err := g.core.Consume(ctx, b, authorization, credential.ManageCredentials, ContactResource(d)); err != nil {
			return credential.Challenge{}, err
		}
	} else {
		contacts, err := accounts.ListCredentialContacts(ctx, user.UUID)
		if err != nil {
			return credential.Challenge{}, credential.ErrProviderUnavailable
		}
		passkey, err := passkeycredential.Read(ctx, user.UUID)
		if err != nil {
			return credential.Challenge{}, credential.ErrProviderUnavailable
		}
		if user.TwoFactor != "" || len(contacts) != 0 || len(passkey.Records) != 0 {
			return credential.Challenge{}, credential.ErrUnauthenticated
		}
		if err := g.core.ChargeAttempt(ctx, b); err != nil {
			return credential.Challenge{}, err
		}
		if !accounts.VerifyCurrentPassword(user, password) {
			return credential.Challenge{}, credential.ErrVerificationFailed
		}
	}
	// Only this Go action sets the intent. Generic BeginVerification requests
	// cannot enroll a destination or substitute their own proof of ownership.
	ctx = context.WithValue(ctx, bindingIntentKey{}, bindingIntent{b, d})
	return g.core.Begin(ctx, b, credential.Request{ProviderID: "native:bind_" + d.Kind + "_otp", Purpose: credential.BindContact, Resource: ContactResource(d)})
}

func (g *Gateway) SaveContact(ctx context.Context, d notificationotp.Destination, token string) (models.CredentialContact, error) {
	b, user, err := identity(ctx)
	if err != nil {
		return models.CredentialContact{}, err
	}
	d, err = normalizeContact(d)
	if err != nil {
		return models.CredentialContact{}, err
	}
	a, err := g.core.Consume(ctx, b, token, credential.BindContact, ContactResource(d))
	if err != nil {
		return models.CredentialContact{}, err
	}
	if a.ProviderID != "native:bind_"+d.Kind+"_otp" {
		return models.CredentialContact{}, credential.ErrVerificationFailed
	}
	contact := models.CredentialContact{Kind: d.Kind, Address: d.Address, Transport: d.Transport}
	if err := accounts.ChangeVerifiedContact(ctx, user, contact); err != nil {
		return models.CredentialContact{}, contactError(err)
	}
	g.core.RevokeUser(user.UUID)
	contacts, err := accounts.ListCredentialContacts(ctx, user.UUID)
	if err != nil {
		return models.CredentialContact{}, credential.ErrProviderUnavailable
	}
	for _, c := range contacts {
		if c.Kind == d.Kind {
			return c, nil
		}
	}
	return models.CredentialContact{}, credential.ErrProviderUnavailable
}

func (g *Gateway) DeleteContact(ctx context.Context, kind, token string) (bool, error) {
	if kind != "email" && kind != "telegram" {
		return false, credential.ErrInvalidArgument
	}
	b, user, err := identity(ctx)
	if err != nil {
		return false, err
	}
	if _, err := g.core.Consume(ctx, b, token, credential.ManageCredentials, DeleteContactResource(kind)); err != nil {
		return false, err
	}
	deleted, err := accounts.RemoveCredentialContact(ctx, user, kind)
	if err != nil {
		return false, contactError(err)
	}
	g.core.RevokeUser(user.UUID)
	return deleted, nil
}

func contactError(err error) error {
	if errors.Is(err, accounts.ErrLastCredential) {
		return accounts.ErrLastCredential
	}
	if errors.Is(err, accounts.ErrCredentialStateChanged) {
		return credential.ErrUnauthenticated
	}
	return credential.ErrProviderUnavailable
}

type contactBindingProvider struct {
	gateway *Gateway
	kind    string
}

func (p *contactBindingProvider) Descriptor() credential.Descriptor {
	return credential.Descriptor{ID: "native:bind_" + p.kind + "_otp", DisplayName: "Verify new " + p.kind, Kind: p.kind + "_otp", Purposes: []credential.Purpose{credential.BindContact}}
}
func (p *contactBindingProvider) Available(ctx context.Context, b credential.Binding) (bool, error) {
	intent, ok := ctx.Value(bindingIntentKey{}).(bindingIntent)
	return ok && intent.binding == b && intent.destination.Kind == p.kind, nil
}
func (p *contactBindingProvider) Begin(ctx context.Context, a credential.Attempt) (credential.Prompt, credential.VerifyFunc, error) {
	intent, ok := ctx.Value(bindingIntentKey{}).(bindingIntent)
	if !ok || intent.binding != a.Binding || intent.destination.Kind != p.kind || a.Request.Purpose != credential.BindContact || a.Request.Resource != ContactResource(intent.destination) {
		return credential.Prompt{}, nil, credential.ErrProviderUnavailable
	}
	return p.gateway.otp.Prepare(ctx, a, intent.destination)
}
