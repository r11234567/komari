// Package signonauth isolates login-settings authorization from plugin RPC,
// API keys and workspace grants. Only a fresh interactive 2FA check mints it.
package signonauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"github.com/komari-monitor/komari/database/accounts"
	"github.com/komari-monitor/komari/pkg/rpc"
)

const CookieName = "komari_sign_on_grant"
const Lifetime = 30 * time.Minute

var ErrLocked = errors.New("login settings locked: fresh interactive 2FA is required")

type binding struct {
	user, session string
	secret        [32]byte
}
type grant struct {
	binding
	expires time.Time
}
type store struct {
	mu     sync.Mutex
	grants map[[32]byte]grant
	now    func() time.Time
	valid  func(binding) bool
}

var grants = newStore(time.Now, func(b binding) bool {
	id, _, err := accounts.GetSessionIdentity(b.session)
	if err != nil || id != b.user {
		return false
	}
	user, err := accounts.GetUserByUUID(id)
	return err == nil && user.TwoFactor != "" && sha256.Sum256([]byte(user.TwoFactor)) == b.secret
})

func newStore(now func() time.Time, valid func(binding) bool) *store {
	return &store{grants: make(map[[32]byte]grant), now: now, valid: valid}
}

func (s *store) create(b binding) (string, error) {
	if b.user == "" || b.session == "" || !s.valid(b) {
		return "", ErrLocked
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	token := hex.EncodeToString(raw[:])
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, g := range s.grants {
		if !s.now().Before(g.expires) || g.session == b.session {
			delete(s.grants, key)
		}
	}
	if len(s.grants) >= 512 {
		return "", errors.New("too many login-settings grants")
	}
	s.grants[sha256.Sum256([]byte(token))] = grant{binding: b, expires: s.now().Add(Lifetime)}
	return token, nil
}

func (s *store) check(token, user, session string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := sha256.Sum256([]byte(token))
	g, ok := s.grants[key]
	if !ok || g.user != user || g.session != session {
		return ErrLocked
	}
	if !s.now().Before(g.expires) || !s.valid(g.binding) {
		delete(s.grants, key)
		return ErrLocked
	}
	return nil
}

// Create must only be called after RequireSensitive2FA has consumed a fresh code.
func Create(p *rpc.Principal, session string) (string, error) {
	if p == nil || p.Type != rpc.PrincipalUser || p.IsAPIKey || !p.HasRole(rpc.RoleAdmin) {
		return "", ErrLocked
	}
	user, err := accounts.GetUserByUUID(p.UserUUID)
	if err != nil {
		return "", ErrLocked
	}
	return grants.create(binding{user: p.UserUUID, session: session, secret: sha256.Sum256([]byte(user.TwoFactor))})
}

type tokenKey struct{}

func WithToken(ctx context.Context, token string) context.Context {
	return context.WithValue(ctx, tokenKey{}, token)
}

func Require(ctx context.Context) error {
	meta := rpc.MetaFromContext(ctx)
	if meta == nil || meta.Principal == nil || meta.Principal.Type != rpc.PrincipalUser || meta.Principal.IsAPIKey || !meta.Principal.HasRole(rpc.RoleAdmin) {
		return ErrLocked
	}
	token, _ := ctx.Value(tokenKey{}).(string)
	return grants.check(token, meta.Principal.UserUUID, meta.SessionToken)
}
