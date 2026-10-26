package signonauth

import (
	"context"
	"testing"
	"time"

	"github.com/komari-monitor/komari/pkg/rpc"
)

func TestGrantSessionExpiryAndRevocation(t *testing.T) {
	now := time.Now()
	valid := true
	s := newStore(func() time.Time { return now }, func(binding) bool { return valid })
	b := binding{user: "user", session: "session"}
	token, err := s.create(b)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.check(token, b.user, b.session); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ token, user, session string }{
		{"workspace-token", b.user, b.session}, {token, "other-user", b.session}, {token, b.user, "other-session"},
	} {
		if s.check(test.token, test.user, test.session) == nil {
			t.Fatal("cross-scope/session token accepted")
		}
	}
	newToken, err := s.create(b)
	if err != nil {
		t.Fatal(err)
	}
	if s.check(token, b.user, b.session) == nil {
		t.Fatal("older grant survived replacement")
	}
	now = now.Add(Lifetime)
	if s.check(newToken, b.user, b.session) == nil {
		t.Fatal("expired grant accepted")
	}
	token, err = s.create(b)
	if err != nil {
		t.Fatal(err)
	}
	valid = false
	if s.check(token, b.user, b.session) == nil {
		t.Fatal("revoked session/2FA accepted")
	}
}

func TestOnlyInteractiveAdminsCanUseLoginSettingsGrant(t *testing.T) {
	for _, p := range []*rpc.Principal{nil, rpc.NewAnonymousPrincipal(), rpc.NewAgentPrincipal("node"), rpc.NewAPIKeyPrincipal(), rpc.PrincipalFromRole(rpc.RoleAdmin)} {
		ctx := rpc.NewContextWithMeta(context.Background(), &rpc.ContextMeta{Principal: p})
		if Require(WithToken(ctx, "plugin-or-workspace-token")) == nil {
			t.Fatalf("accepted %v", p)
		}
	}
}
