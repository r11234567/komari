package credentialgateway

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/komari-monitor/komari/cmd/flags"
	"github.com/komari-monitor/komari/database/accounts"
	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/internal/notificationotp"
	credential "github.com/komari-monitor/komari/internal/trustedcredential"
	"github.com/komari-monitor/komari/pkg/rpc"
	"github.com/komari-monitor/komari/utils/messageSender/factory"
	"github.com/pquerna/otp/totp"
)

func TestMain(m *testing.M) {
	root, err := os.MkdirTemp("", "komari-native-contacts-*")
	if err != nil {
		panic(err)
	}
	flags.DatabaseType = flags.DatabaseTypeSQLite
	flags.DatabaseFile = filepath.Join(root, "synthetic.db")
	if err := dbcore.Initialize(); err != nil {
		panic(err)
	}
	code := m.Run()
	_ = dbcore.Close()
	_ = os.RemoveAll(root)
	os.Exit(code)
}

type contactFixture struct {
	gateway *Gateway
	ctx     context.Context
	user    models.User
	now     time.Time
	codes   map[string]string
	sends   int
}

func newContactFixture(t *testing.T, withTOTP bool) *contactFixture {
	t.Helper()
	user, err := accounts.CreateAccount("synthetic-contacts-"+uuid.NewString(), "Synthetic-password-123")
	if err != nil {
		t.Fatal(err)
	}
	if withTOTP {
		if err := accounts.Enable2Fa(user.UUID, "JBSWY3DPEHPK3PXP"); err != nil {
			t.Fatal(err)
		}
		user, err = accounts.GetUserByUUID(user.UUID)
		if err != nil {
			t.Fatal(err)
		}
	}
	session := "synthetic-contact-session-" + uuid.NewString()
	db := dbcore.GetDBInstance()
	if err := db.Create(&models.Session{UUID: user.UUID, Session: session, Expires: time.Now().Add(time.Hour)}).Error; err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"email", "telegram"} {
		if err := db.Save(&models.MessageSenderProvider{Name: kind, Addition: `{}`}).Error; err != nil {
			t.Fatal(err)
		}
	}
	f := &contactFixture{user: user, now: time.Now(), codes: map[string]string{}}
	f.ctx = rpc.NewContextWithMeta(context.Background(), &rpc.ContextMeta{Principal: rpc.NewUserPrincipal(user.UUID), UserUUID: user.UUID, SessionToken: session})
	f.gateway = newGateway(notificationotp.New(notificationotp.Options{Now: func() time.Time { return f.now }, Deliver: func(_ context.Context, _ string, d factory.CredentialDelivery) error {
		f.codes[d.Address] = strings.Split(strings.Split(d.Message, "is: ")[1], "\n")[0]
		f.sends++
		return nil
	}}))
	t.Cleanup(func() {
		f.gateway.RevokeUser(user.UUID)
		db.Where("user_uuid = ?", user.UUID).Delete(&models.CredentialContact{})
		db.Where("uuid = ?", user.UUID).Delete(&models.Session{})
		db.Where("uuid = ?", user.UUID).Delete(&models.User{})
	})
	return f
}
func (f *contactFixture) complete(t *testing.T, c credential.Challenge, address string) credential.Authorization {
	t.Helper()
	raw, _ := json.Marshal(map[string]string{"code": f.codes[address]})
	a, err := f.gateway.Complete(f.ctx, c.ID, raw)
	if err != nil {
		t.Fatal(err)
	}
	return a
}
func (f *contactFixture) totpAuthorization(t *testing.T, resource string) credential.Authorization {
	t.Helper()
	c, err := f.gateway.Begin(f.ctx, credential.Request{ProviderID: "native:totp", Purpose: credential.ManageCredentials, Resource: resource})
	if err != nil {
		t.Fatal(err)
	}
	code, err := totp.GenerateCode("JBSWY3DPEHPK3PXP", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]string{"code": code})
	a, err := f.gateway.Complete(f.ctx, c.ID, raw)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestPasswordBootstrapRequiresNewAddressOTPAndRotatesSecurityState(t *testing.T) {
	f := newContactFixture(t, false)
	g := f.gateway
	d := notificationotp.Destination{Kind: "email", Address: "private@example.test", Transport: "email"}
	if _, err := g.BeginContactBinding(f.ctx, d, "", "wrong"); err != credential.ErrVerificationFailed || f.sends != 0 {
		t.Fatal("password confirmation missing")
	}
	c, err := g.BeginContactBinding(f.ctx, d, "", "Synthetic-password-123")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.SaveContact(f.ctx, d, strings.Repeat("a", 64)); err == nil {
		t.Fatal("unverified contact saved")
	}
	settings, err := g.Contacts(f.ctx)
	if err != nil || len(settings.Contacts) != 0 || !settings.PasswordBootstrapAllowed {
		t.Fatal("pending contact persisted")
	}
	a := f.complete(t, c, d.Address)
	other := d
	other.Address = "different@example.test"
	if _, err := g.SaveContact(f.ctx, other, a.Token); err == nil {
		t.Fatal("destination changed after verification")
	}
	if _, err := g.SaveContact(f.ctx, d, a.Token); err != nil {
		t.Fatal(err)
	}
	settings, err = g.Contacts(f.ctx)
	if err != nil || len(settings.Contacts) != 1 || settings.PasswordBootstrapAllowed || settings.Contacts[0].VerifiedAt.IsZero() {
		t.Fatal("verified binding missing")
	}
	current, err := accounts.GetUserByUUID(f.user.UUID)
	if err != nil || current.CredentialVersion == "" || current.CredentialVersion == f.user.CredentialVersion {
		t.Fatal("security version not rotated")
	}
	if _, err := g.SaveContact(f.ctx, d, a.Token); err == nil {
		t.Fatal("binding grant replayed")
	}
	if _, err := g.BeginContactBinding(f.ctx, other, "", "Synthetic-password-123"); err != credential.ErrUnauthenticated {
		t.Fatal("password replaced existing second factor")
	}
	providers, err := g.List(f.ctx, credential.ManageCredentials)
	if err != nil || len(providers) != 1 || providers[0].ID != "native:email_otp" {
		t.Fatal("verified native provider unavailable")
	}
}

func TestExistingTOTPAndServerOnlyBindingIntent(t *testing.T) {
	f := newContactFixture(t, true)
	g := f.gateway
	d := notificationotp.Destination{Kind: "telegram", Address: "123456789", Transport: "telegram"}
	if _, err := g.BeginContactBinding(f.ctx, d, "", "Synthetic-password-123"); err != credential.ErrUnauthenticated {
		t.Fatal("existing TOTP bypassed")
	}
	if _, err := g.Begin(f.ctx, credential.Request{ProviderID: "native:bind_telegram_otp", Purpose: credential.BindContact, Resource: ContactResource(d)}); err != credential.ErrProviderUnavailable {
		t.Fatal("generic begin created binding intent")
	}
	a := f.totpAuthorization(t, ContactResource(d))
	c, err := g.BeginContactBinding(f.ctx, d, a.Token, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.BeginContactBinding(f.ctx, d, a.Token, ""); err == nil {
		t.Fatal("existing proof reused")
	}
	verified := f.complete(t, c, d.Address)
	if _, err := g.SaveContact(f.ctx, d, verified.Token); err != nil {
		t.Fatal(err)
	}
}

func TestLastContactCannotBeDeletedWithoutAnotherFactor(t *testing.T) {
	f := newContactFixture(t, false)
	g := f.gateway
	d := notificationotp.Destination{Kind: "email", Address: "only@example.test", Transport: "email"}
	c, err := g.BeginContactBinding(f.ctx, d, "", "Synthetic-password-123")
	if err != nil {
		t.Fatal(err)
	}
	a := f.complete(t, c, d.Address)
	if _, err := g.SaveContact(f.ctx, d, a.Token); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(time.Minute)
	c, err = g.Begin(f.ctx, credential.Request{ProviderID: "native:email_otp", Purpose: credential.ManageCredentials, Resource: DeleteContactResource("email")})
	if err != nil {
		t.Fatal(err)
	}
	a = f.complete(t, c, d.Address)
	if _, err := g.DeleteContact(f.ctx, "email", a.Token); !errors.Is(err, accounts.ErrLastCredential) {
		t.Fatalf("last factor: %v", err)
	}
	settings, err := g.Contacts(f.ctx)
	if err != nil || len(settings.Contacts) != 1 {
		t.Fatal("last contact removed")
	}
}

func TestBindingChangeInvalidatesOtherPendingChallenges(t *testing.T) {
	f := newContactFixture(t, true)
	g := f.gateway
	d := notificationotp.Destination{Kind: "email", Address: "new@example.test", Transport: "email"}
	pending, err := g.Begin(f.ctx, credential.Request{ProviderID: "native:totp", Purpose: credential.LoginSettings, Resource: "account"})
	if err != nil {
		t.Fatal(err)
	}
	a := f.totpAuthorization(t, ContactResource(d))
	c, err := g.BeginContactBinding(f.ctx, d, a.Token, "")
	if err != nil {
		t.Fatal(err)
	}
	a = f.complete(t, c, d.Address)
	if _, err := g.SaveContact(f.ctx, d, a.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Complete(f.ctx, pending.ID, json.RawMessage(`{"code":"000000"}`)); err == nil {
		t.Fatal("old challenge retained after contact change")
	}
}

func TestBootstrapPasswordAttemptsAreBoundedAcrossChallenges(t *testing.T) {
	f := newContactFixture(t, false)
	d := notificationotp.Destination{Kind: "email", Address: "private@example.test", Transport: "email"}
	for range credential.MaxUserAttempts {
		if _, err := f.gateway.BeginContactBinding(f.ctx, d, "", "wrong"); err != credential.ErrVerificationFailed {
			t.Fatal(err)
		}
	}
	if _, err := f.gateway.BeginContactBinding(f.ctx, d, "", "Synthetic-password-123"); !errors.Is(err, credential.ErrLimit) {
		t.Fatal("bootstrap attempt limit missing")
	}
	if f.sends != 0 {
		t.Fatal("unconfirmed password sent OTP")
	}
}
