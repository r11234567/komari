package remotemanagement

import (
	"context"
	"testing"
	"time"

	"github.com/komari-monitor/komari/web/workspace"

	websshv1 "github.com/r11234567/komari-proto/gen/go/komari/webssh/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func resetSessions() {
	sessions.Lock()
	defer sessions.Unlock()
	for _, session := range sessions.values {
		session.Close(websshv1.CloseReason_CLOSE_REASON_CANCELLED)
	}
	sessions.values = make(map[string]*Session)
	sessions.notify = make(map[string]chan struct{})
}

func TestSessionLeaseCommandAndEventLifecycle(t *testing.T) {
	resetSessions()
	t.Cleanup(resetSessions)
	session, err := Create("node-a", "admin-a", "", "", &websshv1.TerminalSize{Rows: 24, Columns: 80})
	if err != nil {
		t.Fatal(err)
	}
	assignment, err := Lease(context.Background(), "node-a")
	if err != nil {
		t.Fatal(err)
	}
	attached, err := Attach("node-a", assignment.AssignmentId, assignment.SessionId)
	if err != nil || attached != session {
		t.Fatalf("attach: session=%p err=%v", attached, err)
	}
	if accepted, err := session.EnqueueCommand(1, Input("whoami\n")); err != nil || accepted != 1 {
		t.Fatalf("enqueue: accepted=%d err=%v", accepted, err)
	}
	command, err := session.NextCommand(context.Background(), 0)
	if err != nil || string(command.GetInput()) != "whoami\n" {
		t.Fatalf("command: %#v err=%v", command, err)
	}
	if _, err := session.AppendAgentEvent(&websshv1.AgentSessionEvent{
		Sequence: 1, AcceptedCommandSequence: 1, OccurredAt: timestamppb.Now(),
		Event: &websshv1.AgentSessionEvent_Output{Output: []byte("admin-a\n")},
	}); err != nil {
		t.Fatal(err)
	}
	event, err := session.NextEvent(context.Background(), 0)
	if err != nil || string(event.GetOutput()) != "admin-a\n" {
		t.Fatalf("event: %#v err=%v", event, err)
	}
	if accepted, err := session.AcknowledgeEvents(event.Sequence); err != nil || accepted != event.Sequence {
		t.Fatalf("acknowledge: accepted=%d err=%v", accepted, err)
	}
	if len(session.events) != 0 || session.bufferedOutput != 0 {
		t.Fatalf("acknowledged replay buffer was retained: events=%d bytes=%d", len(session.events), session.bufferedOutput)
	}
	closed := session.Close(websshv1.CloseReason_CLOSE_REASON_CANCELLED)
	if closed.SessionId != session.ID {
		t.Fatalf("closed session = %q, want %q", closed.SessionId, session.ID)
	}
}

func TestSessionRejectsCrossOwnerAndCrossAgent(t *testing.T) {
	resetSessions()
	t.Cleanup(resetSessions)
	session, err := Create("node-a", "admin-a", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := GetOwned(session.ID, "admin-b"); err != ErrForbidden {
		t.Fatalf("cross-owner error = %v", err)
	}
	assignment, err := Lease(context.Background(), "node-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Attach("node-b", assignment.AssignmentId, assignment.SessionId); err != ErrInvalidLease {
		t.Fatalf("cross-agent attach error = %v", err)
	}
}

func TestSessionGrantCancellationAndNormalCloseCleanup(t *testing.T) {
	resetSessions()
	t.Cleanup(resetSessions)
	for _, closeFirst := range []bool{false, true} {
		store := workspace.NewStore(time.Now, func(workspace.Binding) bool { return true })
		token, grant, err := store.Create(workspace.Binding{UserID: "owner", Session: "login", AgentID: "node", SystemUser: "alice"})
		if err != nil {
			t.Fatal(err)
		}
		session, err := Create("node", "owner", "", "", nil, grant.Grant)
		if err != nil {
			t.Fatal(err)
		}
		if closeFirst {
			session.Close(websshv1.CloseReason_CLOSE_REASON_CANCELLED)
		}
		if err := store.Lock(token, "owner", "login"); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, err = session.NextEvent(ctx, 0)
		cancel()
		if err != ErrClosed {
			t.Fatalf("revoked grant did not close terminal: %v", err)
		}
		session.mu.Lock()
		retained := session.grantStop != nil
		reason := session.closed.Reason
		session.mu.Unlock()
		if retained {
			t.Fatal("closed terminal retains grant callback")
		}
		if closeFirst && reason != websshv1.CloseReason_CLOSE_REASON_CANCELLED {
			t.Fatalf("grant callback changed a normal close: %v", reason)
		}
	}
}

func TestWorkspaceLastBrowserConsumerCleanup(t *testing.T) {
	resetSessions()
	t.Cleanup(resetSessions)
	s, err := Create("SJC", "user", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	other, err := Create("FRA", "user", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	a, b := s.RetainBrowser(), s.RetainBrowser()
	defer other.RetainBrowser()()
	a()
	a()
	SweepBrowserConsumers(time.Now().Add(2 * time.Minute))
	if _, err := s.EnqueueCommand(1, Input("pwd\n")); err != nil {
		t.Fatalf("sibling consumer lost session: %v", err)
	}
	b()
	s.mu.Lock()
	deadline := s.browserDeadline
	s.mu.Unlock()
	SweepBrowserConsumers(deadline.Add(-time.Nanosecond))
	if _, err := s.EnqueueCommand(2, Input("whoami\n")); err != nil {
		t.Fatal(err)
	}
	reopened := s.RetainBrowser()
	SweepBrowserConsumers(deadline.Add(time.Second))
	if _, err := s.EnqueueCommand(3, Input("true\n")); err != nil {
		t.Fatalf("reconnect did not cancel cleanup: %v", err)
	}
	reopened()
	s.mu.Lock()
	deadline = s.browserDeadline
	s.mu.Unlock()
	SweepBrowserConsumers(deadline)
	if _, err := s.EnqueueCommand(4, Input("must not execute\n")); err != ErrClosed {
		t.Fatalf("orphan session survived: %v", err)
	}
	command, err := s.NextCommand(context.Background(), 3)
	if err != nil || command.GetCloseReason() == "" {
		t.Fatalf("agent not notified of cleanup: %v %v", command, err)
	}
	if _, err := other.EnqueueCommand(1, Input("pwd\n")); err != nil {
		t.Fatalf("cleanup affected FRA: %v", err)
	}
}
