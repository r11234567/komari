package credentialgateway

import (
	"context"
	"encoding/json"
	"testing"

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
