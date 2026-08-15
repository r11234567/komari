package remotemanagement

import (
	"context"
	"testing"

	websshv1 "github.com/r11234567/komari-proto/gen/go/komari/webssh/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func resetSessions() {
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
