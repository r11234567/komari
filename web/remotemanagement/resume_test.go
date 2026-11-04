package remotemanagement

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/komari-monitor/komari/web/workspace"
	websshv1 "github.com/r11234567/komari-proto/gen/go/komari/webssh/v1"
)

func TestTerminalResumptionNeverLeasesAnotherPTY(t *testing.T) {
	resetSessions()
	t.Cleanup(resetSessions)
	store := workspace.NewStore(time.Now, func(workspace.Binding) bool { return true })
	_, grant, err := store.Create(workspace.Binding{UserID: "owner", Session: "login", AgentID: "node", SystemUser: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	s, err := Create("node", "owner", "", "", nil, grant.Grant)
	if err != nil {
		t.Fatal(err)
	}
	a, err := Lease(context.Background(), "node")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Attach("node", a.AssignmentId, a.SessionId, true); err != nil {
		t.Fatal(err)
	}
	f := s.TransportFrame(nil)
	if f.ResumeUntil.AsTime().After(grant.IdleExpiresAt) || f.ResumeUntil.AsTime().After(grant.Grant.ExpiresAt) {
		t.Fatal("transport extended authorization")
	}
	_, _ = s.AppendAgentEvent(&websshv1.AgentSessionEvent{Sequence: 1, Event: &websshv1.AgentSessionEvent_Output{Output: []byte("original")}})
	s.Detach()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err = Lease(ctx, "node"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second PTY assignment: %v", err)
	}
	if resumed, err := Attach("node", a.AssignmentId, a.SessionId, true); err != nil || resumed != s {
		t.Fatalf("original session not retained: %v", err)
	}
	if frame := s.TransportFrame(nil); frame.AcceptedEventSequence != 1 {
		t.Fatal("lost output checkpoint")
	}
	s.Detach()
	s.mu.Lock()
	s.resumeUntil = time.Now().Add(-time.Second)
	s.mu.Unlock()
	SweepBrowserConsumers(time.Now())
	if _, err = Attach("node", a.AssignmentId, a.SessionId, true); err == nil {
		t.Fatal("expired shell resumed")
	}
}

func TestLegacyDetachFailsClosedRatherThanRestartingShell(t *testing.T) {
	resetSessions()
	t.Cleanup(resetSessions)
	s, _ := Create("node", "owner", "", "", nil)
	a, err := Lease(context.Background(), "node")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Attach("node", a.AssignmentId, a.SessionId); err != nil {
		t.Fatal(err)
	}
	s.Detach()
	if _, err = Attach("node", a.AssignmentId, a.SessionId); err == nil {
		t.Fatal("legacy transport could restart its shell")
	}
	event, err := s.NextEvent(context.Background(), 0)
	if err != nil || event.GetClosed() == nil {
		t.Fatal("browser was not informed of session loss")
	}
}
