package credentialgateway

import (
	"context"

	"github.com/komari-monitor/komari/database/accounts"
	"github.com/komari-monitor/komari/internal/credentialaction"
	"github.com/komari-monitor/komari/internal/passkeycredential"
	credential "github.com/komari-monitor/komari/internal/trustedcredential"
)

func init() { credentialaction.Install(Default) }

// SessionState validates the actual stored session before deriving its opaque
// credential fingerprint. It creates neither a principal nor a new session.
func (g *Gateway) SessionState(ctx context.Context, user, session string) (string, error) {
	id, created, err := accounts.GetSessionIdentity(session)
	if err != nil || id == "" || id != user {
		return "", credential.ErrUnauthenticated
	}
	u, err := accounts.GetUserByUUID(id)
	if err != nil {
		return "", credential.ErrUnauthenticated
	}
	binding, err := deriveBinding(ctx, u, session, created)
	return binding.SecurityVersion, err
}

func (g *Gateway) HasSecondFactor(ctx context.Context, user string) (bool, error) {
	u, err := accounts.GetUserByUUID(user)
	if err != nil {
		return true, credential.ErrUnauthenticated
	}
	contacts, err := accounts.ListCredentialContacts(ctx, user)
	if err != nil {
		return true, credential.ErrProviderUnavailable
	}
	passkey, err := passkeycredential.Read(ctx, user)
	if err != nil {
		return true, credential.ErrProviderUnavailable
	}
	return u.TwoFactor != "" || len(contacts) > 0 || len(passkey.Records) > 0, nil
}
