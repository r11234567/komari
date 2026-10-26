// Package credentialgateway is the common interactive authentication boundary
// for Connect and internal RPC. A role-only plugin principal is not a session.
package credentialgateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/komari-monitor/komari/database/accounts"
	"github.com/komari-monitor/komari/database/models"
	credential "github.com/komari-monitor/komari/internal/trustedcredential"
	"github.com/komari-monitor/komari/pkg/rpc"
)

type Gateway struct{ core *credential.Service }

var Default = New()

func New() *Gateway {
	g := &Gateway{core: credential.New(credential.Options{ValidateBinding: validateBinding})}
	if err := g.core.Register(totpProvider{}); err != nil {
		panic("invalid native credential provider")
	}
	return g
}

func identity(ctx context.Context) (credential.Binding, models.User, error) {
	if ctx == nil || ctx.Err() != nil {
		return credential.Binding{}, models.User{}, credential.ErrUnauthenticated
	}
	m := rpc.MetaFromContext(ctx)
	if m == nil || m.Principal == nil || m.Principal.Type != rpc.PrincipalUser || m.Principal.IsAPIKey || !m.Principal.HasRole(rpc.RoleAdmin) || m.Principal.UserUUID == "" || m.UserUUID != m.Principal.UserUUID || m.SessionToken == "" {
		return credential.Binding{}, models.User{}, credential.ErrUnauthenticated
	}
	id, created, err := accounts.GetSessionIdentity(m.SessionToken)
	if err != nil || id != m.UserUUID {
		return credential.Binding{}, models.User{}, credential.ErrUnauthenticated
	}
	user, err := accounts.GetUserByUUID(id)
	if err != nil {
		return credential.Binding{}, models.User{}, credential.ErrUnauthenticated
	}
	// Length-delimited encoding prevents ambiguous concatenation. Secret inputs
	// are hashed, remain server-side and invalidate pending work after changes.
	version, _ := json.Marshal([]string{user.Passwd, user.TwoFactor, created.UTC().Format(time.RFC3339Nano)})
	sessionHash, versionHash := sha256.Sum256([]byte(m.SessionToken)), sha256.Sum256(version)
	return credential.Binding{UserID: id, SessionDigest: hex.EncodeToString(sessionHash[:]), SecurityVersion: hex.EncodeToString(versionHash[:])}, user, nil
}

func validateBinding(ctx context.Context, binding credential.Binding) error {
	current, _, err := identity(ctx)
	if err != nil || current != binding {
		return credential.ErrUnauthenticated
	}
	return nil
}

func (g *Gateway) List(ctx context.Context, purpose credential.Purpose) ([]credential.Descriptor, error) {
	b, _, err := identity(ctx)
	if err != nil {
		return nil, err
	}
	return g.core.List(ctx, b, purpose)
}

func (g *Gateway) Begin(ctx context.Context, request credential.Request) (credential.Challenge, error) {
	b, _, err := identity(ctx)
	if err != nil {
		return credential.Challenge{}, err
	}
	return g.core.Begin(ctx, b, request)
}

func (g *Gateway) Complete(ctx context.Context, id string, proof json.RawMessage) (credential.Authorization, error) {
	b, _, err := identity(ctx)
	if err != nil {
		return credential.Authorization{}, err
	}
	return g.core.Complete(ctx, b, id, proof)
}

func (g *Gateway) Cancel(ctx context.Context, id string) (bool, error) {
	b, _, err := identity(ctx)
	if err != nil {
		return false, err
	}
	return g.core.Cancel(ctx, b, id)
}

// Consume is Go-only. Consumers provide the purpose and authoritative target;
// the public verification service cannot register providers or consume grants.
func (g *Gateway) Consume(ctx context.Context, token string, purpose credential.Purpose, resource string) (credential.Authorization, error) {
	b, _, err := identity(ctx)
	if err != nil {
		return credential.Authorization{}, err
	}
	return g.core.Consume(ctx, b, token, purpose, resource)
}

func (g *Gateway) RegisterProvider(provider credential.Provider) error {
	return g.core.Register(provider)
}
func (g *Gateway) UnregisterProvider(id string) { g.core.Unregister(id) }
func (g *Gateway) RevokeUser(id string)         { g.core.RevokeUser(id) }
