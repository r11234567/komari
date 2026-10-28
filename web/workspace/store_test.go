package workspace

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestGrantDeadlinesAndPolling(t *testing.T) {
	now := time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)
	store := NewStore(func() time.Time { return now }, func(Binding) bool { return true })
	binding := Binding{"user", "session", "node", "alice"}
	token, snapshot, err := store.Create(binding)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(IdleLifetime - time.Nanosecond)
	if _, err := store.Check(token, "user", "session", "node", false); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Nanosecond)
	if _, err := store.Check(token, "user", "session", "node", false); err == nil {
		t.Fatal("poll extended idle deadline")
	}
	if snapshot.Grant.Context().Err() == nil {
		t.Fatal("expired grant did not cancel ongoing work")
	}
	token, snapshot, err = store.Create(binding)
	if err != nil {
		t.Fatal(err)
	}
	for now.Before(snapshot.Grant.ExpiresAt) {
		remaining := snapshot.Grant.ExpiresAt.Sub(now)
		step := time.Minute
		if remaining < step {
			step = remaining
		}
		now = now.Add(step)
		_, err = store.Check(token, "user", "session", "node", true)
		if now.Before(snapshot.Grant.ExpiresAt) && err != nil {
			t.Fatal(err)
		}
	}
	if err == nil {
		t.Fatal("foreground activity extended absolute deadline")
	}
}

func TestGrantScopeRevocationAndConcurrentChecks(t *testing.T) {
	valid := true
	store := NewStore(time.Now, func(Binding) bool { return valid })
	token, snapshot, err := store.Create(Binding{"user", "session", "node", "alice"})
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range []Binding{{UserID: "other", Session: "session", AgentID: "node"}, {UserID: "user", Session: "other", AgentID: "node"}, {UserID: "user", Session: "session", AgentID: "other"}} {
		if _, err := store.Check(token, b.UserID, b.Session, b.AgentID, false); err == nil {
			t.Fatal("scope crossed")
		}
	}
	var group sync.WaitGroup
	for i := 0; i < 20; i++ {
		group.Add(1)
		go func() { defer group.Done(); _, _ = store.Check(token, "user", "session", "node", true) }()
	}
	group.Wait()
	ctx, cancel := Bind(context.Background(), snapshot.Grant)
	defer cancel()
	if _, _, err := store.Create(Binding{"user", "session", "node", "bob"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("account switch did not cancel old work")
	}
	if snapshot.Grant.Alive() {
		t.Fatal("old account remains authorized")
	}
	token, snapshot, err = store.Create(Binding{"user", "session", "node", "alice"})
	if err != nil {
		t.Fatal(err)
	}
	valid = false
	store.Sweep()
	if snapshot.Grant.Context().Err() == nil {
		t.Fatal("logout did not revoke grant")
	}
	if _, err := store.Check(token, "user", "session", "node", false); err == nil {
		t.Fatal("revoked login accepted")
	}
}

func TestWorkspaceSlowValidationDoesNotHoldOtherMachineLock(t *testing.T) {
	var block atomic.Bool
	entered, release := make(chan struct{}), make(chan struct{})
	store := NewStore(time.Now, func(b Binding) bool {
		if block.Load() && b.AgentID == "SJC" {
			close(entered)
			<-release
			return false
		}
		return true
	})
	sjc, grant, err := store.Create(Binding{"user", "login", "SJC", "alice"})
	if err != nil {
		t.Fatal(err)
	}
	fra, _, err := store.Create(Binding{"user", "login", "FRA", "alice"})
	if err != nil {
		t.Fatal(err)
	}
	block.Store(true)
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	checked := make(chan error, 1)
	go func() { _, err := store.Check(sjc, "user", "login", "SJC", true); checked <- err }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("validation did not block")
	}
	locked := make(chan error, 1)
	go func() { locked <- store.Lock(fra, "user", "login") }()
	select {
	case err := <-locked:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("SJC validation held FRA state mutex")
	}
	if grant.Grant.Context().Err() != nil {
		t.Fatal("locking FRA revoked SJC")
	}
	// Cleanup unblocks validation via defer; the final result is observed below
	// by a separate explicit close so there are no leaked test goroutines.
	block.Store(false)
	releaseOnce.Do(func() { close(release) })
	select {
	case err := <-checked:
		if err == nil {
			t.Fatal("failed validation accepted")
		}
	case <-time.After(time.Second):
		t.Fatal("validation did not finish")
	}
	if grant.Grant.Context().Err() == nil {
		t.Fatal("rejected grant remained alive")
	}
}
