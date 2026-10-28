// Package workspace owns short-lived interactive authorization independently of
// RPC2 roles, plugin contexts, account-page unlocks, and API keys.
package workspace

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/komari-monitor/komari/database/accounts"
	"github.com/komari-monitor/komari/database/clients"
	"github.com/komari-monitor/komari/pkg/rpc"
)

const AbsoluteLifetime = 30 * time.Minute
const IdleLifetime = 3 * time.Minute
const Header = "X-Komari-Workspace-Grant"
const IdentityHeader = "X-Komari-Workspace-Identity"

var ErrLocked = errors.New("workspace locked: fresh 2FA and an explicit OS account are required")

type Binding struct{ UserID, Session, AgentID, SystemUser string }
type Grant struct {
	ID            string
	Binding       Binding
	ExpiresAt     time.Time
	ctx           context.Context
	cancel        context.CancelFunc
	store         *Store
	tokenKey      [32]byte
	idleExpiresAt time.Time // guarded by store.mu
}
type Snapshot struct {
	Grant         *Grant
	IdleExpiresAt time.Time
}
type Store struct {
	mu           sync.Mutex
	grants       map[[32]byte]*Grant
	now          func() time.Time
	sessionValid func(Binding) bool
}

// DiagnosticCounts does not validate sessions, sweep grants, or read a database.
// Stored grants can include entries awaiting the periodic sweep.
func (s *Store) DiagnosticCounts() map[string]int {
	if !s.mu.TryLock() {
		return map[string]int{"snapshot_busy": 1}
	}
	defer s.mu.Unlock()
	return map[string]int{"stored_grants": len(s.grants)}
}

func NewStore(now func() time.Time, valid func(Binding) bool) *Store {
	return &Store{grants: make(map[[32]byte]*Grant), now: now, sessionValid: valid}
}

var Default = NewStore(time.Now, func(b Binding) bool {
	id, _, err := accounts.GetSessionIdentity(b.Session)
	if err != nil || id != b.UserID {
		return false
	}
	allowed, err := clients.PrivilegedCapabilityAllowed(b.AgentID, "webssh")
	return err == nil && allowed
})

func init() {
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for range ticker.C {
			Default.Sweep()
		}
	}()
}

// Create is called only after the Connect boundary consumes fresh 2FA.
func (s *Store) Create(b Binding) (string, Snapshot, error) {
	if b.UserID == "" || b.Session == "" || strings.TrimSpace(b.AgentID) == "" || b.SystemUser == "" || b.SystemUser != strings.TrimSpace(b.SystemUser) || len(b.SystemUser) > 128 || strings.ContainsAny(b.SystemUser, "\x00\r\n") || !s.sessionValid(b) {
		return "", Snapshot{}, ErrLocked
	}
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return "", Snapshot{}, err
	}
	token := hex.EncodeToString(secret[:])
	key := sha256.Sum256([]byte(token))
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	// Switching account or unlocking again invalidates every older grant for
	// this machine in the same browser session.
	for _, g := range s.grants {
		if g.Binding.Session == b.Session && g.Binding.AgentID == b.AgentID {
			s.removeLocked(g)
		}
	}
	if len(s.grants) >= 256 {
		return "", Snapshot{}, errors.New("too many active workspaces")
	}
	now := s.now()
	ctx, cancel := context.WithCancel(context.Background())
	g := &Grant{ID: uuid.NewString(), Binding: b, ExpiresAt: now.Add(AbsoluteLifetime), idleExpiresAt: now.Add(IdleLifetime), ctx: ctx, cancel: cancel, store: s, tokenKey: key}
	s.grants[key] = g
	return token, Snapshot{g, g.idleExpiresAt}, nil
}

func (s *Store) Check(token, user, session, agent string, touch bool) (Snapshot, error) {
	s.mu.Lock()
	g := s.grants[sha256.Sum256([]byte(token))]
	if g == nil || g.Binding.UserID != user || g.Binding.Session != session || (agent != "" && g.Binding.AgentID != agent) {
		s.mu.Unlock()
		return Snapshot{}, ErrLocked
	}
	if !s.validLocked(g) {
		s.removeLocked(g)
		s.mu.Unlock()
		return Snapshot{}, ErrLocked
	}
	s.mu.Unlock()
	// Database-backed validation must never hold the cross-machine state mutex.
	valid := s.sessionValid(g.Binding)
	s.mu.Lock()
	defer s.mu.Unlock()
	if !valid || s.grants[g.tokenKey] != g || !s.validLocked(g) {
		s.removeLocked(g)
		return Snapshot{}, ErrLocked
	}
	if touch {
		g.idleExpiresAt = s.now().Add(IdleLifetime)
		if g.idleExpiresAt.After(g.ExpiresAt) {
			g.idleExpiresAt = g.ExpiresAt
		}
	}
	return Snapshot{g, g.idleExpiresAt}, nil
}

func (s *Store) Lock(token, user, session string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	g := s.grants[sha256.Sum256([]byte(token))]
	if g == nil {
		return nil
	}
	if g.Binding.UserID != user || g.Binding.Session != session {
		return ErrLocked
	}
	s.removeLocked(g)
	return nil
}
func (s *Store) Sweep() {
	s.mu.Lock()
	s.sweepLocked()
	candidates := make([]*Grant, 0, len(s.grants))
	for _, g := range s.grants {
		candidates = append(candidates, g)
	}
	s.mu.Unlock()
	for _, g := range candidates {
		_ = g.Alive()
	}
}
func (s *Store) sweepLocked() {
	for _, g := range s.grants {
		if !s.validLocked(g) {
			s.removeLocked(g)
		}
	}
}
func (s *Store) validLocked(g *Grant) bool {
	return s.now().Before(g.ExpiresAt) && s.now().Before(g.idleExpiresAt) && g.ctx.Err() == nil
}
func (s *Store) removeLocked(g *Grant) {
	if s.grants[g.tokenKey] == g {
		delete(s.grants, g.tokenKey)
	}
	g.cancel()
}
func (g *Grant) Context() context.Context { return g.ctx }

// ByID is the Agent-side lease lookup; it never treats an Agent as a browser user.
func (s *Store) ByID(id, agent, username string) (*Grant, error) {
	s.mu.Lock()
	var found *Grant
	for _, g := range s.grants {
		if g.ID == id && g.Binding.AgentID == agent && g.Binding.SystemUser == username {
			found = g
			break
		}
	}
	s.mu.Unlock()
	if found == nil || !found.Alive() {
		return nil, ErrLocked
	}
	return found, nil
}

// Active is for an already authenticated stream only. The existing one-second
// sweep still revalidates login and machine policy; command entry points always
// use Check/Alive and never treat this local liveness test as authorization.
func (g *Grant) Active() bool {
	g.store.mu.Lock()
	defer g.store.mu.Unlock()
	if g.store.grants[g.tokenKey] != g || !g.store.validLocked(g) {
		g.store.removeLocked(g)
		return false
	}
	return true
}
func (g *Grant) Alive() bool {
	if !g.Active() {
		return false
	}
	valid := g.store.sessionValid(g.Binding)
	g.store.mu.Lock()
	defer g.store.mu.Unlock()
	if !valid || g.store.grants[g.tokenKey] != g || !g.store.validLocked(g) {
		g.store.removeLocked(g)
		return false
	}
	return true
}

type tokenContextKey struct{}
type grantContextKey struct{}
type secureContextKey struct{}

func WithSecure(ctx context.Context, secure bool) context.Context {
	return context.WithValue(ctx, secureContextKey{}, secure)
}
func Secure(ctx context.Context) bool { value, _ := ctx.Value(secureContextKey{}).(bool); return value }
func CookieName(agent string) string {
	digest := sha256.Sum256([]byte(agent))
	return "komari_workspace_" + hex.EncodeToString(digest[:8])
}

func WithToken(ctx context.Context, token string) context.Context {
	return context.WithValue(ctx, tokenContextKey{}, token)
}
func Token(ctx context.Context) string {
	value, _ := ctx.Value(tokenContextKey{}).(string)
	return value
}
func FromContext(ctx context.Context) *Grant {
	value, _ := ctx.Value(grantContextKey{}).(*Grant)
	return value
}

func Interactive(meta *rpc.ContextMeta) (string, string, error) {
	if meta == nil || meta.Principal == nil || meta.Principal.Type != rpc.PrincipalUser || meta.Principal.IsAPIKey || !meta.Principal.HasRole(rpc.RoleAdmin) || meta.SessionToken == "" {
		return "", "", ErrLocked
	}
	id, _, err := accounts.GetSessionIdentity(meta.SessionToken)
	if err != nil || id != meta.Principal.UserUUID {
		return "", "", ErrLocked
	}
	return id, meta.SessionToken, nil
}

func Authorize(ctx context.Context, agent string) (context.Context, context.CancelFunc, *Grant, error) {
	user, session, err := Interactive(rpc.MetaFromContext(ctx))
	if err != nil {
		return ctx, func() {}, nil, err
	}
	snapshot, err := Default.Check(Token(ctx), user, session, agent, false)
	if err != nil {
		return ctx, func() {}, nil, err
	}
	bound, cancel := Bind(ctx, snapshot.Grant)
	return bound, cancel, snapshot.Grant, nil
}

// Bind cancels ongoing work when its grant expires, is locked, or its login is revoked.
func Bind(ctx context.Context, g *Grant) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.WithValue(ctx, grantContextKey{}, g))
	stop := context.AfterFunc(g.Context(), cancel)
	if g.Context().Err() != nil {
		cancel()
	}
	return ctx, func() { stop(); cancel() }
}
