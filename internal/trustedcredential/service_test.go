package trustedcredential

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type testProvider struct {
	d     Descriptor
	begin func(context.Context, Attempt) (Prompt, VerifyFunc, error)
}

func (p *testProvider) Descriptor() Descriptor                           { return p.d }
func (p *testProvider) Available(context.Context, Binding) (bool, error) { return true, nil }
func (p *testProvider) Begin(ctx context.Context, a Attempt) (Prompt, VerifyFunc, error) {
	if p.begin != nil {
		return p.begin(ctx, a)
	}
	return Prompt{Type: "otp", Data: json.RawMessage(`{"digits":6}`)}, func(context.Context, json.RawMessage) error { return nil }, nil
}

var testBinding = Binding{UserID: "synthetic-user", SessionDigest: "synthetic-session", SecurityVersion: "synthetic-version"}

func fixture(t *testing.T) (*Service, *testProvider, *time.Time) {
	t.Helper()
	now := time.Now()
	s := New(Options{Now: func() time.Time { return now }, ValidateBinding: func(context.Context, Binding) error { return nil }})
	p := &testProvider{d: Descriptor{ID: "native:test", DisplayName: "Synthetic", Kind: "legacy_totp", Purposes: []Purpose{BindContact, UnlockWorkspace}}}
	if err := s.Register(p); err != nil {
		t.Fatal(err)
	}
	return s, p, &now
}
func beginTest(t *testing.T, s *Service, b Binding) Challenge {
	t.Helper()
	c, err := s.Begin(context.Background(), b, Request{ProviderID: "native:test", Purpose: UnlockWorkspace, Resource: "agent:alice"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func completeTest(t *testing.T, s *Service, b Binding, c Challenge) Authorization {
	t.Helper()
	a, err := s.Complete(context.Background(), b, c.ID, json.RawMessage(`{"code":"synthetic"}`))
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestAuthorizationBindingScopeAndSingleUse(t *testing.T) {
	s, _, _ := fixture(t)
	c := beginTest(t, s, testBinding)
	other := testBinding
	other.SessionDigest = "other-session"
	if _, err := s.Complete(context.Background(), other, c.ID, json.RawMessage(`{}`)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("session isolation: %v", err)
	}
	a := completeTest(t, s, testBinding, c)
	for _, b := range []Binding{other, {UserID: "other-user", SessionDigest: testBinding.SessionDigest, SecurityVersion: testBinding.SecurityVersion}, {UserID: testBinding.UserID, SessionDigest: testBinding.SessionDigest, SecurityVersion: "changed"}} {
		if _, err := s.Consume(context.Background(), b, a.Token, UnlockWorkspace, "agent:alice"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("binding: %v", err)
		}
	}
	for _, scope := range []struct {
		p Purpose
		r string
	}{{BindContact, "agent:alice"}, {UnlockWorkspace, "agent:bob"}} {
		if _, err := s.Consume(context.Background(), testBinding, a.Token, scope.p, scope.r); !errors.Is(err, ErrNotFound) {
			t.Fatalf("scope: %v", err)
		}
	}
	var successes atomic.Int32
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Consume(context.Background(), testBinding, a.Token, UnlockWorkspace, "agent:alice"); err == nil {
				successes.Add(1)
			}
		}()
	}
	wg.Wait()
	if successes.Load() != 1 {
		t.Fatalf("consumed %d times", successes.Load())
	}
	if _, err := s.Complete(context.Background(), testBinding, c.ID, json.RawMessage(`{}`)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("completed challenge retained: %v", err)
	}
}

func TestExactExpiryAndRevocation(t *testing.T) {
	for _, mode := range []string{"challenge-expiry", "grant-expiry", "unregister", "user-revoke", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			s, _, now := fixture(t)
			c := beginTest(t, s, testBinding)
			if mode == "challenge-expiry" {
				*now = c.ExpiresAt
				if _, err := s.Complete(context.Background(), testBinding, c.ID, json.RawMessage(`{}`)); !errors.Is(err, ErrNotFound) {
					t.Fatal(err)
				}
				return
			}
			if mode == "cancel" {
				if ok, err := s.Cancel(context.Background(), testBinding, c.ID); err != nil || !ok {
					t.Fatal(err)
				}
				if _, err := s.Complete(context.Background(), testBinding, c.ID, json.RawMessage(`{}`)); !errors.Is(err, ErrNotFound) {
					t.Fatal(err)
				}
				return
			}
			a := completeTest(t, s, testBinding, c)
			switch mode {
			case "grant-expiry":
				*now = a.ExpiresAt
			case "unregister":
				s.Unregister("native:test")
			case "user-revoke":
				s.RevokeUser(testBinding.UserID)
			}
			if _, err := s.Consume(context.Background(), testBinding, a.Token, UnlockWorkspace, "agent:alice"); !errors.Is(err, ErrNotFound) {
				t.Fatal(err)
			}
		})
	}
}

func TestRegistrySnapshotsAndReplacement(t *testing.T) {
	s, p, _ := fixture(t)
	if err := s.Register(p); !errors.Is(err, ErrInvalidArgument) {
		t.Fatal(err)
	}
	p.d.Purposes[0] = SensitiveOperation
	items, err := s.List(context.Background(), testBinding, BindContact)
	if err != nil || len(items) != 1 {
		t.Fatalf("descriptor changed: %v", err)
	}
	items[0].Purposes[0] = SensitiveOperation
	items, err = s.List(context.Background(), testBinding, BindContact)
	if err != nil || len(items) != 1 || items[0].Purposes[0] != BindContact {
		t.Fatal("returned descriptor aliases registry")
	}
	c := beginTest(t, s, testBinding)
	s.Unregister("native:test")
	if err := s.Register(p); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Complete(context.Background(), testBinding, c.ID, json.RawMessage(`{}`)); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

func TestAttemptBudgetSurvivesNewChallenges(t *testing.T) {
	s, p, now := fixture(t)
	p.begin = func(context.Context, Attempt) (Prompt, VerifyFunc, error) {
		return Prompt{Type: "otp", Data: json.RawMessage(`{}`)}, func(context.Context, json.RawMessage) error {
			return errors.New("private synthetic provider diagnostic")
		}, nil
	}
	for i := 0; i < 2; i++ {
		c := beginTest(t, s, testBinding)
		for range MaxAttempts {
			if _, err := s.Complete(context.Background(), testBinding, c.ID, json.RawMessage(`{}`)); err != ErrVerificationFailed {
				t.Fatalf("non-redacted failure: %v", err)
			}
		}
		if _, err := s.Complete(context.Background(), testBinding, c.ID, json.RawMessage(`{}`)); err != ErrNotFound {
			t.Fatalf("attempt limit: %v", err)
		}
	}
	if _, err := s.Begin(context.Background(), testBinding, Request{ProviderID: "native:test", Purpose: UnlockWorkspace, Resource: "agent:alice"}); err != ErrLimit {
		t.Fatalf("user budget: %v", err)
	}
	*now = now.Add(AttemptWindow)
	beginTest(t, s, testBinding)
}

func TestCancellationDuringVerificationDoesNotAuthorize(t *testing.T) {
	for _, mode := range []string{"cancel", "unregister", "revoke", "context"} {
		t.Run(mode, func(t *testing.T) {
			s, p, _ := fixture(t)
			started, release := make(chan struct{}), make(chan struct{})
			p.begin = func(context.Context, Attempt) (Prompt, VerifyFunc, error) {
				return Prompt{Type: "otp", Data: json.RawMessage(`{}`)}, func(context.Context, json.RawMessage) error { close(started); <-release; return nil }, nil
			}
			c := beginTest(t, s, testBinding)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan error, 1)
			go func() { _, err := s.Complete(ctx, testBinding, c.ID, json.RawMessage(`{}`)); result <- err }()
			<-started
			if _, err := s.Complete(context.Background(), testBinding, c.ID, json.RawMessage(`{}`)); err != ErrBusy {
				t.Fatalf("parallel verifier: %v", err)
			}
			switch mode {
			case "cancel":
				s.Cancel(context.Background(), testBinding, c.ID)
			case "unregister":
				s.Unregister("native:test")
			case "revoke":
				s.RevokeUser(testBinding.UserID)
			case "context":
				cancel()
			}
			close(release)
			if err := <-result; err == nil {
				t.Fatal("revoked in-flight verifier authorized")
			}
			if len(s.grants) != 0 {
				t.Fatal("grant survived revocation")
			}
		})
	}
}

func TestCapacityInputAndMissingValidator(t *testing.T) {
	s, p, _ := fixture(t)
	for range MaxPerSession {
		beginTest(t, s, testBinding)
	}
	if _, err := s.Begin(context.Background(), testBinding, Request{ProviderID: "native:test", Purpose: UnlockWorkspace, Resource: "agent:alice"}); err != ErrLimit {
		t.Fatal(err)
	}
	s.RevokeUser(testBinding.UserID)
	for _, r := range []Request{
		{ProviderID: p.d.ID, Purpose: UnlockWorkspace},
		{ProviderID: p.d.ID, Purpose: "unknown", Resource: "target"},
		{ProviderID: p.d.ID, Purpose: UnlockWorkspace, Resource: "target", Parameters: json.RawMessage(`[]`)},
		{ProviderID: p.d.ID, Purpose: UnlockWorkspace, Resource: strings.Repeat("a", 2049)},
		{ProviderID: p.d.ID, Purpose: UnlockWorkspace, Resource: "target", Parameters: json.RawMessage(`{"x":"` + strings.Repeat("a", MaxParametersBytes) + `"}`)},
	} {
		if _, err := s.Begin(context.Background(), testBinding, r); err != ErrInvalidArgument {
			t.Fatalf("input: %v", err)
		}
	}
	if _, err := New(Options{}).List(context.Background(), testBinding, ""); err != ErrUnauthenticated {
		t.Fatal(err)
	}
	s.random = io.LimitReader(strings.NewReader(""), 0)
	if _, err := s.Begin(context.Background(), testBinding, Request{ProviderID: p.d.ID, Purpose: UnlockWorkspace, Resource: "target"}); err != ErrProviderUnavailable {
		t.Fatal(err)
	}
}

func TestBindingInvalidatedAfterVerification(t *testing.T) {
	s, p, _ := fixture(t)
	var invalid atomic.Bool
	s.validate = func(context.Context, Binding) error {
		if invalid.Load() {
			return ErrUnauthenticated
		}
		return nil
	}
	p.begin = func(context.Context, Attempt) (Prompt, VerifyFunc, error) {
		return Prompt{Type: "otp", Data: json.RawMessage(`{}`)}, func(context.Context, json.RawMessage) error { invalid.Store(true); return nil }, nil
	}
	c := beginTest(t, s, testBinding)
	if _, err := s.Complete(context.Background(), testBinding, c.ID, json.RawMessage(`{}`)); err != ErrUnauthenticated {
		t.Fatal(err)
	}
	if len(s.grants) != 0 {
		t.Fatal("invalidated session got a grant")
	}
}

func TestPreparationFailureReleasesCapacity(t *testing.T) {
	s, p, _ := fixture(t)
	p.begin = func(context.Context, Attempt) (Prompt, VerifyFunc, error) {
		return Prompt{}, nil, errors.New("private diagnostic")
	}
	for range MaxPerSession + 2 {
		if _, err := s.Begin(context.Background(), testBinding, Request{ProviderID: p.d.ID, Purpose: UnlockWorkspace, Resource: "target"}); err != ErrProviderUnavailable {
			t.Fatalf("preparation: %v", err)
		}
	}
	if len(s.pending) != 0 {
		t.Fatal("failed preparations leaked")
	}
}
