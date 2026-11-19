package workspace

import (
	"testing"
	"time"
)

func TestTransportDeadlineNeverExtendsHumanAuthorization(t *testing.T) {
	now := time.Now()
	store := NewStore(func() time.Time { return now }, func(Binding) bool { return true })
	token, snapshot, err := store.Create(Binding{"owner", "login", "node", "alice"})
	if err != nil {
		t.Fatal(err)
	}
	grant := snapshot.Grant
	short := now.Add(10 * time.Second)
	if got := grant.TransportDeadline(short); !got.Equal(short) {
		t.Fatalf("transport limit changed: %s", got)
	}
	for range 2 {
		now = now.Add(time.Minute)
		if got := grant.TransportDeadline(now.Add(time.Hour)); !got.Equal(snapshot.IdleExpiresAt) {
			t.Fatalf("heartbeat extended idle authorization: %s", got)
		}
	}
	// Only actual foreground activity may renew idle, never absolute lifetime.
	for i := 0; i < 27; i++ {
		if _, err := store.Check(token, "owner", "login", "node", true); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Minute)
	}
	if got := grant.TransportDeadline(now.Add(time.Hour)); !got.Equal(grant.ExpiresAt) {
		t.Fatalf("transport exceeded absolute authorization: %s", got)
	}
	if err := store.Lock(token, "owner", "login"); err != nil {
		t.Fatal(err)
	}
	if got := grant.TransportDeadline(now.Add(time.Hour)); !got.IsZero() {
		t.Fatalf("revoked authorization retained a transport lease: %s", got)
	}
}
