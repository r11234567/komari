package credentialgateway

import (
	"testing"
	"time"

	"github.com/komari-monitor/komari/internal/credentialaction"
	"github.com/komari-monitor/komari/internal/notificationotp"
	credential "github.com/komari-monitor/komari/internal/trustedcredential"
	"github.com/komari-monitor/komari/pkg/rpc"
)

func TestTrustedActionNativeContactAuthorizations(t *testing.T) {
	f := newContactFixture(t, false)
	g := f.gateway
	d := notificationotp.Destination{Kind: "email", Address: "action@example.test", Transport: "email"}
	c, err := g.BeginContactBinding(f.ctx, d, "", "Synthetic-password-123")
	if err != nil {
		t.Fatal(err)
	}
	a := f.complete(t, c, d.Address)
	if _, err := g.SaveContact(f.ctx, d, a.Token); err != nil {
		t.Fatal(err)
	}
	meta := rpc.MetaFromContext(f.ctx)
	state, err := g.SessionState(f.ctx, meta.UserUUID, meta.SessionToken)
	if err != nil || state == "" {
		t.Fatal("missing live credential state", err)
	}
	configured, err := g.HasSecondFactor(f.ctx, f.user.UUID)
	if err != nil || !configured {
		t.Fatal("native contact not recognized", err)
	}
	for _, action := range []struct {
		purpose  credential.Purpose
		resource string
	}{
		{credential.SensitiveOperation, credentialaction.AccountEntry},
		{credential.LoginSettings, credentialaction.LoginSettings},
		{credential.UnlockWorkspace, credentialaction.WorkspaceResource("synthetic-agent", "alice")},
	} {
		f.now = f.now.Add(time.Minute)
		c, err := g.Begin(f.ctx, credential.Request{ProviderID: "native:email_otp", Purpose: action.purpose, Resource: action.resource})
		if err != nil {
			t.Fatal(err)
		}
		a := f.complete(t, c, d.Address)
		consumed, err := g.Consume(f.ctx, a.Token, action.purpose, action.resource)
		if err != nil || consumed.ProviderID != "native:email_otp" {
			t.Fatal("native authorization was not consumed", err)
		}
	}
}
