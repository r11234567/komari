package trustedcredential

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

var providerIDPattern = regexp.MustCompile(`^[a-z][a-z0-9:._-]{1,95}$`)

type providerEntry struct {
	provider   Provider
	descriptor Descriptor
}

type pending struct {
	binding  Binding
	entry    *providerEntry
	purpose  Purpose
	resource string
	expires  time.Time
	verify   VerifyFunc
	attempts int
	busy     bool
	cancel   context.CancelFunc
}

type authorization struct {
	binding  Binding
	entry    *providerEntry
	purpose  Purpose
	resource string
	expires  time.Time
}

type Service struct {
	mu        sync.Mutex
	now       func() time.Time
	random    io.Reader
	validate  func(context.Context, Binding) error
	providers map[string]*providerEntry
	pending   map[string]*pending
	grants    map[[32]byte]authorization
	attempts  map[string]attemptBudget
}

type attemptBudget struct {
	count   int
	expires time.Time
}

func New(options Options) *Service {
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Random == nil {
		options.Random = rand.Reader
	}
	if options.ValidateBinding == nil {
		options.ValidateBinding = func(context.Context, Binding) error { return ErrUnauthenticated }
	}
	return &Service{now: options.Now, random: options.Random, validate: options.ValidateBinding,
		providers: map[string]*providerEntry{}, pending: map[string]*pending{}, grants: map[[32]byte]authorization{}, attempts: map[string]attemptBudget{}}
}

// Register is deliberately not reachable through a plugin module or transport.
// A server adapter is the authority for the actual verification boundary.
func (s *Service) Register(provider Provider) error {
	if provider == nil {
		return ErrInvalidArgument
	}
	d := provider.Descriptor()
	if !providerIDPattern.MatchString(d.ID) || strings.TrimSpace(d.DisplayName) == "" || len(d.DisplayName) > 128 || !utf8.ValidString(d.DisplayName) || len(d.Purposes) == 0 || len(d.Purposes) > 5 {
		return ErrInvalidArgument
	}
	switch d.Kind {
	case "legacy_totp", "email_otp", "telegram_otp", "plugin":
	default:
		return ErrInvalidArgument
	}
	d.Purposes = slices.Clone(d.Purposes)
	seen := map[Purpose]bool{}
	for _, p := range d.Purposes {
		if !validPurpose(p) || seen[p] {
			return ErrInvalidArgument
		}
		seen[p] = true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.providers[d.ID] != nil {
		return ErrInvalidArgument
	}
	s.providers[d.ID] = &providerEntry{provider: provider, descriptor: d}
	return nil
}

func (s *Service) Unregister(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.providers[id]
	delete(s.providers, id)
	for key, p := range s.pending {
		if p.entry == entry {
			s.removePendingLocked(key, p)
		}
	}
	for key, g := range s.grants {
		if g.entry == entry {
			delete(s.grants, key)
		}
	}
}

func (s *Service) List(ctx context.Context, binding Binding, purpose Purpose) ([]Descriptor, error) {
	if err := s.checkBinding(ctx, binding); err != nil {
		return nil, err
	}
	if purpose != "" && !validPurpose(purpose) {
		return nil, ErrInvalidArgument
	}
	s.mu.Lock()
	entries := make([]*providerEntry, 0, len(s.providers))
	for _, entry := range s.providers {
		if purpose == "" || slices.Contains(entry.descriptor.Purposes, purpose) {
			entries = append(entries, entry)
		}
	}
	s.mu.Unlock()
	result := make([]Descriptor, 0, len(entries))
	for _, entry := range entries {
		available, err := entry.provider.Available(ctx, binding)
		if err != nil {
			return nil, providerError(ctx, err)
		}
		s.mu.Lock()
		active := s.providers[entry.descriptor.ID] == entry
		s.mu.Unlock()
		if active && available {
			d := entry.descriptor
			d.Purposes = slices.Clone(d.Purposes)
			result = append(result, d)
		}
	}
	if err := s.checkBinding(ctx, binding); err != nil {
		return nil, err
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}

func (s *Service) Begin(ctx context.Context, binding Binding, request Request) (Challenge, error) {
	if err := s.checkBinding(ctx, binding); err != nil {
		return Challenge{}, err
	}
	if !validPurpose(request.Purpose) || strings.TrimSpace(request.Resource) == "" || len(request.Resource) > 2048 || !utf8.ValidString(request.Resource) {
		return Challenge{}, ErrInvalidArgument
	}
	if len(request.Parameters) == 0 {
		request.Parameters = json.RawMessage(`{}`)
	}
	if !validObject(request.Parameters, MaxParametersBytes) {
		return Challenge{}, ErrInvalidArgument
	}
	s.mu.Lock()
	s.pruneLocked()
	entry := s.providers[request.ProviderID]
	if entry == nil || !slices.Contains(entry.descriptor.Purposes, request.Purpose) {
		s.mu.Unlock()
		return Challenge{}, ErrProviderUnavailable
	}
	if s.attempts[binding.UserID].count >= MaxUserAttempts {
		s.mu.Unlock()
		return Challenge{}, ErrLimit
	}
	if len(s.pending) >= MaxChallenges || s.pendingCountLocked(binding, false) >= MaxPerSession || s.pendingCountLocked(binding, true) >= MaxPerUser {
		s.mu.Unlock()
		return Challenge{}, ErrLimit
	}
	id, err := s.newTokenLocked()
	if err != nil {
		s.mu.Unlock()
		return Challenge{}, err
	}
	if s.pending[id] != nil {
		s.mu.Unlock()
		return Challenge{}, ErrLimit
	}
	p := &pending{binding: binding, entry: entry, purpose: request.Purpose, resource: request.Resource, expires: s.now().Add(ChallengeLifetime)}
	s.pending[id] = p
	s.mu.Unlock()
	prepared := false
	defer func() {
		if !prepared {
			s.mu.Lock()
			if s.pending[id] == p {
				s.removePendingLocked(id, p)
			}
			s.mu.Unlock()
		}
	}()
	preparing, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	available, err := entry.provider.Available(preparing, binding)
	if err != nil {
		return Challenge{}, providerError(preparing, err)
	}
	if !available {
		return Challenge{}, ErrProviderUnavailable
	}
	request.Parameters = slices.Clone(request.Parameters)
	prompt, verify, err := entry.provider.Begin(preparing, Attempt{ChallengeID: id, Binding: binding, Request: request, ExpiresAt: p.expires})
	if err != nil {
		return Challenge{}, providerError(preparing, err)
	}
	if verify == nil || (prompt.Type != "otp" && prompt.Type != "webauthn") || !validObject(prompt.Data, MaxPromptBytes) {
		return Challenge{}, ErrProviderUnavailable
	}
	if err := s.checkBinding(preparing, binding); err != nil {
		return Challenge{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending[id] != p || s.providers[entry.descriptor.ID] != entry || !s.now().Before(p.expires) {
		return Challenge{}, ErrNotFound
	}
	p.verify = verify
	prepared = true
	return Challenge{ID: id, ExpiresAt: p.expires, Prompt: Prompt{Type: prompt.Type, Data: slices.Clone(prompt.Data)}}, nil
}

func (s *Service) Complete(ctx context.Context, binding Binding, id string, proof json.RawMessage) (Authorization, error) {
	if err := s.checkBinding(ctx, binding); err != nil {
		return Authorization{}, err
	}
	if !validToken(id) || !validObject(proof, MaxProofBytes) {
		return Authorization{}, ErrInvalidArgument
	}
	s.mu.Lock()
	s.pruneLocked()
	p := s.pending[id]
	if p == nil || p.binding != binding {
		s.mu.Unlock()
		return Authorization{}, ErrNotFound
	}
	if p.busy || p.verify == nil {
		s.mu.Unlock()
		return Authorization{}, ErrBusy
	}
	if err := s.chargeAttemptLocked(binding); err != nil {
		s.mu.Unlock()
		return Authorization{}, err
	}
	p.busy = true
	p.attempts++
	verifying, cancel := context.WithDeadline(ctx, p.expires)
	p.cancel = cancel
	s.mu.Unlock()
	defer cancel()
	verificationErr := p.verify(verifying, slices.Clone(proof))
	contextErr := verifying.Err()
	bindingErr := s.checkBinding(ctx, binding)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending[id] != p || s.providers[p.entry.descriptor.ID] != p.entry {
		return Authorization{}, ErrNotFound
	}
	p.cancel = nil
	p.busy = false
	if bindingErr != nil {
		s.removePendingLocked(id, p)
		return Authorization{}, bindingErr
	}
	if !s.now().Before(p.expires) {
		s.removePendingLocked(id, p)
		return Authorization{}, ErrNotFound
	}
	if contextErr != nil {
		if p.attempts >= MaxAttempts {
			s.removePendingLocked(id, p)
		}
		return Authorization{}, contextErr
	}
	if verificationErr != nil {
		if p.attempts >= MaxAttempts {
			s.removePendingLocked(id, p)
		}
		if errors.Is(verificationErr, ErrProviderUnavailable) {
			return Authorization{}, ErrProviderUnavailable
		}
		return Authorization{}, ErrVerificationFailed
	}
	s.pruneLocked()
	if len(s.grants) >= MaxAuthorizations || s.grantCountLocked(binding, false) >= MaxPerSession || s.grantCountLocked(binding, true) >= MaxPerUser {
		s.removePendingLocked(id, p)
		return Authorization{}, ErrLimit
	}
	token, err := s.newTokenLocked()
	if err != nil {
		s.removePendingLocked(id, p)
		return Authorization{}, err
	}
	key := sha256.Sum256([]byte(token))
	if _, exists := s.grants[key]; exists {
		s.removePendingLocked(id, p)
		return Authorization{}, ErrLimit
	}
	expires := s.now().Add(AuthorizationLifetime)
	s.grants[key] = authorization{binding: binding, entry: p.entry, purpose: p.purpose, resource: p.resource, expires: expires}
	s.removePendingLocked(id, p)
	return Authorization{Token: token, ProviderID: p.entry.descriptor.ID, Purpose: p.purpose, Resource: p.resource, ExpiresAt: expires}, nil
}

// Consume is server-only. The action supplies its own purpose and authoritative
// resource; an authorization cannot be exchanged for an unrelated operation.
func (s *Service) Consume(ctx context.Context, binding Binding, token string, purpose Purpose, resource string) (Authorization, error) {
	if err := s.checkBinding(ctx, binding); err != nil {
		return Authorization{}, err
	}
	if !validToken(token) || !validPurpose(purpose) {
		return Authorization{}, ErrInvalidArgument
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
	key := sha256.Sum256([]byte(token))
	g, exists := s.grants[key]
	if !exists || g.binding != binding || g.purpose != purpose || g.resource != resource || s.providers[g.entry.descriptor.ID] != g.entry {
		return Authorization{}, ErrNotFound
	}
	delete(s.grants, key)
	return Authorization{ProviderID: g.entry.descriptor.ID, Purpose: g.purpose, Resource: g.resource, ExpiresAt: g.expires}, nil
}

func (s *Service) Cancel(ctx context.Context, binding Binding, id string) (bool, error) {
	if err := s.checkBinding(ctx, binding); err != nil {
		return false, err
	}
	if !validToken(id) {
		return false, ErrInvalidArgument
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
	p := s.pending[id]
	if p == nil || p.binding != binding {
		return false, nil
	}
	s.removePendingLocked(id, p)
	return true, nil
}

func (s *Service) RevokeUser(userID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, p := range s.pending {
		if p.binding.UserID == userID {
			s.removePendingLocked(key, p)
		}
	}
	for key, g := range s.grants {
		if g.binding.UserID == userID {
			delete(s.grants, key)
		}
	}
}

// ChargeAttempt is server-only and shares the same user-wide budget with proof
// completion. Use it before an initial password enrollment confirmation.
func (s *Service) ChargeAttempt(ctx context.Context, binding Binding) error {
	if err := s.checkBinding(ctx, binding); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
	return s.chargeAttemptLocked(binding)
}

func (s *Service) chargeAttemptLocked(binding Binding) error {
	budget, exists := s.attempts[binding.UserID]
	if budget.count >= MaxUserAttempts || (!exists && len(s.attempts) >= MaxAttemptBuckets) {
		return ErrLimit
	}
	if !exists {
		budget.expires = s.now().Add(AttemptWindow)
	}
	budget.count++
	s.attempts[binding.UserID] = budget
	return nil
}

func (s *Service) checkBinding(ctx context.Context, b Binding) error {
	if ctx == nil || b.UserID == "" || b.SessionDigest == "" || b.SecurityVersion == "" {
		return ErrUnauthenticated
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.validate(ctx, b); err != nil {
		return ErrUnauthenticated
	}
	return nil
}

func (s *Service) newTokenLocked() (string, error) {
	var raw [32]byte
	if _, err := io.ReadFull(s.random, raw[:]); err != nil {
		return "", ErrProviderUnavailable
	}
	return hex.EncodeToString(raw[:]), nil
}

func (s *Service) removePendingLocked(id string, p *pending) {
	delete(s.pending, id)
	if p.cancel != nil {
		p.cancel()
	}
}

func (s *Service) pruneLocked() {
	now := s.now()
	for user, budget := range s.attempts {
		if !now.Before(budget.expires) {
			delete(s.attempts, user)
		}
	}
	for key, p := range s.pending {
		if !now.Before(p.expires) {
			s.removePendingLocked(key, p)
		}
	}
	for key, g := range s.grants {
		if !now.Before(g.expires) {
			delete(s.grants, key)
		}
	}
}

func (s *Service) pendingCountLocked(b Binding, user bool) int {
	n := 0
	for _, p := range s.pending {
		if p.binding == b || (user && p.binding.UserID == b.UserID) {
			n++
		}
	}
	return n
}

func (s *Service) grantCountLocked(b Binding, user bool) int {
	n := 0
	for _, g := range s.grants {
		if g.binding == b || (user && g.binding.UserID == b.UserID) {
			n++
		}
	}
	return n
}

func validObject(raw json.RawMessage, limit int) bool {
	if len(raw) == 0 || len(raw) > limit {
		return false
	}
	var obj map[string]json.RawMessage
	return json.Unmarshal(raw, &obj) == nil && obj != nil
}

func validToken(token string) bool {
	if len(token) != 64 {
		return false
	}
	_, err := hex.DecodeString(token)
	return err == nil
}

func providerError(ctx context.Context, err error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if errors.Is(err, ErrLimit) {
		return ErrLimit
	}
	return ErrProviderUnavailable
}
