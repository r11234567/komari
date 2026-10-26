package credentialgateway

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/komari-monitor/komari/database/accounts"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/internal/passkeycredential"
	credential "github.com/komari-monitor/komari/internal/trustedcredential"
	"github.com/komari-monitor/komari/pkg/config"
)

const PasswordLoginLifetime = 3 * time.Minute
const LoginSessionLifetime = 86400
const passwordLoginResource = "login:password"

type passwordContextKey struct{}
type passwordContext struct {
	gateway *Gateway
	token   string
}
type passwordPending struct {
	binding          credential.Binding
	created, expires time.Time
}
type passwordBudget struct {
	count   int
	expires time.Time
}
type passwordLogins struct {
	mu      sync.Mutex
	pending map[[32]byte]passwordPending
	budgets map[[32]byte]passwordBudget
}

func newPasswordLogins() *passwordLogins {
	return &passwordLogins{pending: map[[32]byte]passwordPending{}, budgets: map[[32]byte]passwordBudget{}}
}

type PasswordStart struct {
	// Tokens are Go-only transport values. Never serialize them in response JSON.
	Preauthentication string `json:"-"`
	Session           string `json:"-"`
	UserID            string `json:"-"`
	Providers         []credential.Descriptor
	ExpiresAt         time.Time
}

func passwordLoginEnabled() bool {
	disabled, err := config.GetAs[bool](config.DisablePasswordLoginKey, false)
	return err == nil && !disabled
}

func (g *Gateway) passwordIdentity(ctx context.Context, token string) (credential.Binding, models.User, error) {
	if len(token) != 64 || !passwordLoginEnabled() {
		return credential.Binding{}, models.User{}, credential.ErrUnauthenticated
	}
	key := sha256.Sum256([]byte(token))
	g.password.mu.Lock()
	state, ok := g.password.pending[key]
	g.password.mu.Unlock()
	if !ok || !time.Now().Before(state.expires) {
		return credential.Binding{}, models.User{}, credential.ErrUnauthenticated
	}
	user, err := accounts.GetUserByUUID(state.binding.UserID)
	if err != nil {
		return credential.Binding{}, models.User{}, credential.ErrUnauthenticated
	}
	b, err := deriveBinding(ctx, user, "password-login:"+token, state.created)
	if err != nil || b != state.binding {
		return credential.Binding{}, models.User{}, credential.ErrUnauthenticated
	}
	return b, user, nil
}

func (g *Gateway) passwordContext(ctx context.Context, token string) context.Context {
	return context.WithValue(ctx, passwordContextKey{}, passwordContext{g, token})
}

// BeginPassword requires the password before disclosing factors or sending any
// notification. No live session exists while a second factor is pending.
func (g *Gateway) BeginPassword(ctx context.Context, username, password, remoteIP, userAgent string) (PasswordStart, error) {
	if ctx.Err() != nil {
		return PasswordStart{}, ctx.Err()
	}
	if !passwordLoginEnabled() {
		return PasswordStart{}, credential.ErrUnauthenticated
	}
	if len(username) == 0 || len(username) > 256 || len(password) == 0 || len(password) > 1024 {
		return PasswordStart{}, credential.ErrInvalidArgument
	}
	if !g.reservePasswordAttempt(username, remoteIP) {
		return PasswordStart{}, credential.ErrLimit
	}
	id, ok := accounts.CheckPassword(username, password)
	if !ok {
		return PasswordStart{}, credential.ErrVerificationFailed
	}
	user, err := accounts.GetUserByUUID(id)
	if err != nil {
		return PasswordStart{}, credential.ErrUnauthenticated
	}
	contacts, err := accounts.ListCredentialContacts(ctx, id)
	if err != nil {
		return PasswordStart{}, credential.ErrProviderUnavailable
	}
	passkey, err := passkeycredential.Read(ctx, id)
	if err != nil {
		return PasswordStart{}, err
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return PasswordStart{}, credential.ErrProviderUnavailable
	}
	token := hex.EncodeToString(raw[:])
	now := time.Now().UTC()
	b, err := deriveBinding(ctx, user, "password-login:"+token, now)
	if err != nil {
		return PasswordStart{}, err
	}
	key := sha256.Sum256([]byte(token))
	g.password.mu.Lock()
	count := 0
	for k, state := range g.password.pending {
		if !now.Before(state.expires) {
			delete(g.password.pending, k)
			g.core.RevokeBinding(state.binding)
		} else if state.binding.UserID == id {
			count++
		}
	}
	if len(g.password.pending) >= 256 || count >= 4 {
		g.password.mu.Unlock()
		return PasswordStart{}, credential.ErrLimit
	}
	g.password.pending[key] = passwordPending{binding: b, created: now, expires: now.Add(PasswordLoginLifetime)}
	g.password.mu.Unlock()
	pctx := g.passwordContext(ctx, token)
	result := PasswordStart{Preauthentication: token, UserID: id, ExpiresAt: now.Add(PasswordLoginLifetime)}
	if user.TwoFactor == "" && len(contacts) == 0 && len(passkey.Records) == 0 {
		result.Session, result.UserID, err = g.finishPassword(pctx, token, userAgent, remoteIP, "password")
		result.Preauthentication = ""
		return result, err
	}
	result.Providers, err = g.core.List(pctx, b, credential.PasswordLogin)
	if err != nil || len(result.Providers) == 0 {
		g.CancelPassword(token)
		return PasswordStart{}, credential.ErrProviderUnavailable
	}
	return result, nil
}

func (g *Gateway) BeginPasswordVerification(ctx context.Context, token, provider string) (credential.Challenge, error) {
	ctx = g.passwordContext(ctx, token)
	b, _, err := identity(ctx)
	if err != nil {
		return credential.Challenge{}, err
	}
	// Switching factors replaces this flow's previous challenge; send and proof
	// budgets remain shared and do not reset.
	g.core.RevokeBinding(b)
	return g.core.Begin(ctx, b, credential.Request{ProviderID: provider, Purpose: credential.PasswordLogin, Resource: passwordLoginResource})
}

func (g *Gateway) CompletePassword(ctx context.Context, token, id string, proof json.RawMessage, remoteIP, userAgent string) (string, string, error) {
	ctx = g.passwordContext(ctx, token)
	b, _, err := identity(ctx)
	if err != nil {
		return "", "", err
	}
	a, err := g.core.Complete(ctx, b, id, proof)
	if err != nil {
		return "", "", err
	}
	if _, err := g.core.Consume(ctx, b, a.Token, credential.PasswordLogin, passwordLoginResource); err != nil {
		return "", "", err
	}
	return g.finishPassword(ctx, token, userAgent, remoteIP, "password+"+a.ProviderID)
}

func (g *Gateway) finishPassword(ctx context.Context, token, userAgent, remoteIP, method string) (string, string, error) {
	b, _, err := identity(ctx)
	if err != nil {
		return "", "", err
	}
	key := sha256.Sum256([]byte(token))
	g.password.mu.Lock()
	state, ok := g.password.pending[key]
	if ok && state.binding == b && time.Now().Before(state.expires) {
		delete(g.password.pending, key)
	} else {
		ok = false
	}
	g.password.mu.Unlock()
	if !ok {
		return "", "", credential.ErrUnauthenticated
	}
	g.core.RevokeBinding(b)
	session, err := accounts.CreateSession(b.UserID, LoginSessionLifetime, userAgent, remoteIP, method)
	if err != nil {
		return "", "", credential.ErrProviderUnavailable
	}
	return session, b.UserID, nil
}

func (g *Gateway) CancelPassword(token string) bool {
	key := sha256.Sum256([]byte(token))
	g.password.mu.Lock()
	state, ok := g.password.pending[key]
	delete(g.password.pending, key)
	g.password.mu.Unlock()
	if ok {
		g.core.RevokeBinding(state.binding)
	}
	return ok
}

func (g *Gateway) reservePasswordAttempt(username, remoteIP string) bool {
	keys := [][32]byte{sha256.Sum256([]byte("name:" + strings.ToLower(username))), sha256.Sum256([]byte("ip:" + remoteIP))}
	now := time.Now()
	g.password.mu.Lock()
	defer g.password.mu.Unlock()
	for key, b := range g.password.budgets {
		if !now.Before(b.expires) {
			delete(g.password.budgets, key)
		}
	}
	missing := 0
	for i, key := range keys {
		b, ok := g.password.budgets[key]
		limit := 10
		if i == 1 {
			limit = 20
		}
		if b.count >= limit {
			return false
		}
		if !ok {
			missing++
		}
	}
	if len(g.password.budgets)+missing > 2048 {
		return false
	}
	for _, key := range keys {
		b, ok := g.password.budgets[key]
		if !ok {
			b.expires = now.Add(credential.AttemptWindow)
		}
		b.count++
		g.password.budgets[key] = b
	}
	return true
}
