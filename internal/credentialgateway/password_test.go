package credentialgateway

import (
	"context"
	"encoding/json"
	credential "github.com/komari-monitor/komari/internal/trustedcredential"
	"sync"
	"testing"
	"time"

	"github.com/komari-monitor/komari/database/accounts"
	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/models"
)

func TestPasswordThenNativeOTPLogin(t *testing.T) {
	f := newContactFixture(t, false)
	address := "synthetic-login@example.test"
	if err := accounts.ChangeVerifiedContact(context.Background(), f.user, models.CredentialContact{Kind: "email", Address: address, Transport: "email"}); err != nil {
		t.Fatal(err)
	}
	db := dbcore.GetDBInstance()
	var before int64
	if err := db.Model(&models.Session{}).Where("uuid = ?", f.user.UUID).Count(&before).Error; err != nil {
		t.Fatal(err)
	}
	started, err := f.gateway.BeginPassword(context.Background(), f.user.Username, "Synthetic-password-123", "192.0.2.10", "synthetic-ci")
	if err != nil {
		t.Fatal(err)
	}
	if started.Session != "" || started.Preauthentication == "" || len(started.Providers) != 1 || started.Providers[0].ID != "native:email_otp" || f.sends != 0 {
		t.Fatal("password phase did not remain isolated")
	}
	var after int64
	if err := db.Model(&models.Session{}).Where("uuid = ?", f.user.UUID).Count(&after).Error; err != nil || after != before {
		t.Fatal("password phase created a live session")
	}
	c, err := f.gateway.BeginPasswordVerification(context.Background(), started.Preauthentication, "native:email_otp")
	if err != nil {
		t.Fatal(err)
	}
	proof, _ := json.Marshal(map[string]string{"code": f.codes[address]})
	session, user, err := f.gateway.CompletePassword(context.Background(), started.Preauthentication, c.ID, proof, "192.0.2.10", "synthetic-ci")
	if err != nil || session == "" || user != f.user.UUID {
		t.Fatal("OTP did not finish login", err)
	}
	id, _, err := accounts.GetSessionIdentity(session)
	if err != nil || id != user {
		t.Fatal("completed session unavailable", err)
	}
	if len(f.gateway.password.pending) != 0 {
		t.Fatal("completed preauthentication retained")
	}
}

func TestPasswordOnlyCompatibilityAndCancelledPendingState(t *testing.T) {
	f := newContactFixture(t, false)
	started, err := f.gateway.BeginPassword(context.Background(), f.user.Username, "Synthetic-password-123", "192.0.2.11", "synthetic-ci")
	if err != nil || started.Session == "" || started.Preauthentication != "" {
		t.Fatal("no-factor installation lost compatibility", err)
	}
	factor := newContactFixture(t, true)
	pending, err := factor.gateway.BeginPassword(context.Background(), factor.user.Username, "Synthetic-password-123", "192.0.2.12", "synthetic-ci")
	if err != nil || pending.Session != "" || len(pending.Providers) != 1 || pending.Providers[0].ID != "native:totp" {
		t.Fatal("TOTP fallback metadata missing", err)
	}
	if !factor.gateway.CancelPassword(pending.Preauthentication) || len(factor.gateway.password.pending) != 0 {
		t.Fatal("cancellation did not clear pending state")
	}
}

func TestPasswordResumeSelectsEmailAndReusesChallenge(t *testing.T) {
	f := newContactFixture(t, true)
	address := "synthetic-resume@example.test"
	if err := accounts.ChangeVerifiedContact(context.Background(), f.user, models.CredentialContact{Kind: "email", Address: address, Transport: "email"}); err != nil {
		t.Fatal(err)
	}
	started, err := f.gateway.BeginPassword(context.Background(), f.user.Username, "Synthetic-password-123", "192.0.2.30", "synthetic-ci")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	challenges := make(chan credential.Challenge, 8)
	errors := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := f.gateway.BeginPasswordVerification(context.Background(), started.Preauthentication, "")
			challenges <- c
			errors <- err
		}()
	}
	wg.Wait()
	close(challenges)
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	id := ""
	for c := range challenges {
		if id == "" {
			id = c.ID
		}
		if c.ID != id {
			t.Fatal("navigation replaced the pending challenge")
		}
	}
	if f.sends != 1 || f.codes[address] == "" {
		t.Fatal("default factor was not email or refresh resent OTP")
	}
	proof, _ := json.Marshal(map[string]string{"code": f.codes[address]})
	session, _, err := f.gateway.CompletePassword(context.Background(), started.Preauthentication, id, proof, "192.0.2.30", "synthetic-ci")
	if err != nil || session == "" {
		t.Fatal("resumed OTP did not complete", err)
	}
	if _, err := f.gateway.BeginPasswordVerification(context.Background(), started.Preauthentication, ""); err == nil {
		t.Fatal("completed ceremony resumed")
	}
}

func TestPasswordResumeRejectsMissingCancelledAndExpiredState(t *testing.T) {
	f := newContactFixture(t, true)
	if _, err := f.gateway.BeginPasswordVerification(context.Background(), "", ""); err == nil {
		t.Fatal("missing password cookie accepted")
	}
	start := func() string {
		result, err := f.gateway.BeginPassword(context.Background(), f.user.Username, "Synthetic-password-123", "192.0.2.31", "synthetic-ci")
		if err != nil {
			t.Fatal(err)
		}
		return result.Preauthentication
	}
	cancelled := start()
	f.gateway.CancelPassword(cancelled)
	if _, err := f.gateway.BeginPasswordVerification(context.Background(), cancelled, ""); err == nil {
		t.Fatal("cancelled ceremony resumed")
	}
	expired := start()
	key := opaqueDigest("password-preauth", expired)
	f.gateway.password.mu.Lock()
	state := f.gateway.password.pending[key]
	state.expires = time.Now().Add(-time.Second)
	f.gateway.password.pending[key] = state
	f.gateway.password.mu.Unlock()
	if _, err := f.gateway.BeginPasswordVerification(context.Background(), expired, ""); err == nil {
		t.Fatal("expired ceremony resumed")
	}
}
