package notificationotp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	credential "github.com/komari-monitor/komari/internal/trustedcredential"
	"github.com/komari-monitor/komari/utils/messageSender/factory"
)

func otpAttempt(now time.Time) credential.Attempt {
	return credential.Attempt{ChallengeID: "synthetic-challenge", Binding: credential.Binding{UserID: "synthetic-user", SessionDigest: "synthetic-session", SecurityVersion: "synthetic-security"}, Request: credential.Request{Purpose: credential.UnlockWorkspace, Resource: "synthetic-agent:alice"}, ExpiresAt: now.Add(credential.ChallengeLifetime)}
}
func syntheticDestination() Destination {
	return Destination{Kind: "email", Address: "user@example.test", Transport: "synthetic-email"}
}

func TestNativeOTPIsDeliveredPrivatelyAndConsumedByCore(t *testing.T) {
	now := time.Now()
	var code string
	var delivered factory.CredentialDelivery
	engine := New(Options{Now: func() time.Time { return now }, Deliver: func(_ context.Context, transport string, d factory.CredentialDelivery) error {
		if transport != "synthetic-email" {
			t.Fatal("wrong transport")
		}
		delivered = d
		code = strings.Split(strings.Split(d.Message, "is: ")[1], "\n")[0]
		return nil
	}})
	svc := credential.New(credential.Options{Now: func() time.Time { return now }, ValidateBinding: func(context.Context, credential.Binding) error { return nil }})
	provider := &Provider{Kind: "email", Engine: engine, Resolve: func(context.Context, credential.Binding) (Destination, error) { return syntheticDestination(), nil }}
	if err := svc.Register(provider); err != nil {
		t.Fatal(err)
	}
	a := otpAttempt(now)
	c, err := svc.Begin(context.Background(), a.Binding, credential.Request{ProviderID: "native:email_otp", Purpose: a.Request.Purpose, Resource: a.Request.Resource})
	if err != nil {
		t.Fatal(err)
	}
	if delivered.Address != "user@example.test" || delivered.Kind != "email" || len(code) != 6 {
		t.Fatal("invalid addressed delivery")
	}
	if strings.Contains(string(c.Prompt.Data), code) || strings.Contains(string(c.Prompt.Data), delivered.Address) {
		t.Fatal("public prompt exposed private material")
	}
	proof, _ := json.Marshal(map[string]string{"code": code})
	grant, err := svc.Complete(context.Background(), a.Binding, c.ID, proof)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Complete(context.Background(), a.Binding, c.ID, proof); err != credential.ErrNotFound {
		t.Fatal("completed OTP challenge remained usable")
	}
	if _, err := svc.Consume(context.Background(), a.Binding, grant.Token, a.Request.Purpose, a.Request.Resource); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Consume(context.Background(), a.Binding, grant.Token, a.Request.Purpose, a.Request.Resource); err != credential.ErrNotFound {
		t.Fatal("OTP grant consumed twice")
	}
}

func TestNativeOTPCooldownAndWindowCannotBeResetByNewChallenge(t *testing.T) {
	now := time.Now()
	var sends int
	e := New(Options{Now: func() time.Time { return now }, Deliver: func(context.Context, string, factory.CredentialDelivery) error { sends++; return nil }})
	for i := 0; i < MaxSendsPerWindow; i++ {
		a := otpAttempt(now)
		if _, _, err := e.Prepare(context.Background(), a, syntheticDestination()); err != nil {
			t.Fatal(err)
		}
		a.ChallengeID = "another-challenge"
		if _, _, err := e.Prepare(context.Background(), a, syntheticDestination()); err != credential.ErrLimit {
			t.Fatalf("cooldown: %v", err)
		}
		now = now.Add(SendCooldown)
	}
	if _, _, err := e.Prepare(context.Background(), otpAttempt(now), syntheticDestination()); err != credential.ErrLimit {
		t.Fatalf("window: %v", err)
	}
	if sends != MaxSendsPerWindow {
		t.Fatal("extra messages sent")
	}
	now = now.Add(SendWindow)
	if _, _, err := e.Prepare(context.Background(), otpAttempt(now), syntheticDestination()); err != nil {
		t.Fatal(err)
	}
}

func TestNativeOTPConcurrentSendingHasOneReservation(t *testing.T) {
	now := time.Now()
	var sends atomic.Int32
	e := New(Options{Now: func() time.Time { return now }, Deliver: func(context.Context, string, factory.CredentialDelivery) error { sends.Add(1); return nil }})
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() { defer wg.Done(); e.Prepare(context.Background(), otpAttempt(now), syntheticDestination()) }()
	}
	wg.Wait()
	if sends.Load() != 1 {
		t.Fatal("concurrent send limit exceeded")
	}
}

func TestNativeOTPExpiryRedactionAndPrivateTargets(t *testing.T) {
	now := time.Now()
	e := New(Options{Now: func() time.Time { return now }, Deliver: func(context.Context, string, factory.CredentialDelivery) error {
		return errors.New("synthetic-private-transport-details")
	}})
	if _, _, err := e.Prepare(context.Background(), otpAttempt(now), syntheticDestination()); err != credential.ErrProviderUnavailable {
		t.Fatalf("unredacted error: %v", err)
	}
	if _, _, err := e.Prepare(context.Background(), otpAttempt(now), syntheticDestination()); err != credential.ErrLimit {
		t.Fatal("delivery failure reset cooldown")
	}
	now = now.Add(SendWindow)
	e.deliver = func(context.Context, string, factory.CredentialDelivery) error { return nil }
	a := otpAttempt(now)
	_, verify, err := e.Prepare(context.Background(), a, syntheticDestination())
	if err != nil {
		t.Fatal(err)
	}
	for _, proof := range []json.RawMessage{json.RawMessage(`{"code":""}`), json.RawMessage(`{"code":"１２３４５６"}`), json.RawMessage(`{"code":123456}`), json.RawMessage(`[]`)} {
		if verify(context.Background(), proof) != credential.ErrVerificationFailed {
			t.Fatal("invalid OTP proof accepted")
		}
	}
	now = a.ExpiresAt
	if verify(context.Background(), json.RawMessage(`{"code":"000000"}`)) != credential.ErrVerificationFailed {
		t.Fatal("expired proof accepted")
	}
	for _, d := range []Destination{{Kind: "telegram", Address: "-1234", Transport: "telegram"}, {Kind: "email", Address: "a@example.test,b@example.test", Transport: "email"}} {
		if _, _, err := e.Prepare(context.Background(), otpAttempt(now), d); err != credential.ErrProviderUnavailable {
			t.Fatal("non-private destination accepted")
		}
	}
}

func TestNativeOTPSendBudgetCapacity(t *testing.T) {
	now := time.Now()
	e := New(Options{Now: func() time.Time { return now }})
	for i := 0; i < MaxSendBuckets; i++ {
		var key [32]byte
		key[0] = byte(i)
		key[1] = byte(i >> 8)
		e.budgets[key] = sendBudget{Expires: now.Add(SendWindow)}
	}
	if e.reserve("new-user", syntheticDestination()) {
		t.Fatal("budget capacity exceeded")
	}
}
