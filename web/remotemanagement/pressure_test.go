package remotemanagement

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/komari-monitor/komari/web/workspace"
	websshv1 "github.com/r11234567/komari-proto/gen/go/komari/webssh/v1"
)

func TestWorkspaceRevocationClearsOutputPressureAndQueuedCommands(t *testing.T) {
	resetSessions()
	t.Cleanup(resetSessions)
	store := workspace.NewStore(time.Now, func(workspace.Binding) bool { return true })
	token, grant, err := store.Create(workspace.Binding{UserID: "owner", Session: "login", AgentID: "node", SystemUser: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	s, err := Create("node", "owner", "", "", nil, grant.Grant)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.EnqueueCommand(1, Input("do-not-replay")); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AppendAgentEvent(&websshv1.AgentSessionEvent{Sequence: 1, Event: &websshv1.AgentSessionEvent_Output{Output: make([]byte, 4<<20)}}); err != nil {
		t.Fatal(err)
	}
	if err = store.Lock(token, "owner", "login"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		s.mu.Lock()
		closed := s.closed != nil
		s.mu.Unlock()
		if closed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("revocation did not close buffered session")
		}
		time.Sleep(time.Millisecond)
	}
	command, err := s.NextCommand(context.Background(), 0)
	if err != nil || command.GetCloseReason() == "" || len(command.GetInput()) != 0 {
		t.Fatalf("revoked command dispatched: %v %v", command, err)
	}
	if _, err = s.NextEvent(context.Background(), 0); !errors.Is(err, ErrClosed) {
		t.Fatalf("revoked output retained: %v", err)
	}
	if s.bufferedOutput != 0 || s.bufferedCommandBytes != 0 || s.bufferedEventBytes != 0 {
		t.Fatal("revoked session retains buffers")
	}
}

func TestEventMetadataIsBudgetedAndInvalidCloseDoesNotChangeState(t *testing.T) {
	resetSessions()
	t.Cleanup(resetSessions)
	s, err := Create("node", "owner", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.AppendAgentEvent(&websshv1.AgentSessionEvent{Sequence: 1, Event: &websshv1.AgentSessionEvent_Closed{}}); err == nil {
		t.Fatal("empty close accepted")
	}
	if s.agentSeq != 0 || s.bufferedOutput != 0 || s.bufferedEventBytes != 0 {
		t.Fatal("invalid event mutated buffer")
	}
	metadata := strings.Repeat("x", 1<<20)
	for sequence := uint64(1); sequence <= 15; sequence++ {
		if _, err = s.AppendAgentEvent(&websshv1.AgentSessionEvent{Sequence: sequence, Event: &websshv1.AgentSessionEvent_File{File: &websshv1.FileEvent{Error: metadata}}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = s.AppendAgentEvent(&websshv1.AgentSessionEvent{Sequence: 16, Event: &websshv1.AgentSessionEvent_File{File: &websshv1.FileEvent{Error: metadata}}}); !errors.Is(err, ErrOutputLimit) {
		t.Fatalf("unbounded metadata accepted: %v", err)
	}
	if s.bufferedEventBytes > 1024 {
		t.Fatal("failed close retains oversized metadata")
	}
	if _, err = s.AcknowledgeEvents(s.agentSeq); err != nil {
		t.Fatal(err)
	}
	if s.bufferedEventBytes != 0 {
		t.Fatal("acknowledgment retained event byte budget")
	}
}

func TestCommandBacklogBoundsBytesAndReleasesAcknowledgedStorage(t *testing.T) {
	resetSessions()
	t.Cleanup(resetSessions)
	s, err := Create("node", "owner", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 1<<20)
	if _, err = s.EnqueueCommand(1, Input(data)); err != nil {
		t.Fatal(err)
	}
	if _, err = s.EnqueueCommand(2, Input(data)); !errors.Is(err, ErrCommandBacklog) {
		t.Fatalf("byte budget: %v", err)
	}
	backing := s.commands[:cap(s.commands)]
	if _, err = s.AppendAgentEvent(&websshv1.AgentSessionEvent{AcceptedCommandSequence: 1}); err != nil {
		t.Fatal(err)
	}
	if s.bufferedCommandBytes != 0 || backing[0] != nil {
		t.Fatal("ack retained input allocation")
	}
	if _, err = s.EnqueueCommand(2, Input(data)); err != nil {
		t.Fatal(err)
	}
	s.Close(websshv1.CloseReason_CLOSE_REASON_CANCELLED)
	if s.bufferedCommandBytes != 0 {
		t.Fatal("close retained command budget")
	}
}

func TestOutputPressureClosesShellAndReportsFailure(t *testing.T) {
	for _, tiny := range []bool{false, true} {
		resetSessions()
		t.Cleanup(resetSessions)
		s, err := Create("node", "owner", "", "", nil)
		if err != nil {
			t.Fatal(err)
		}
		chunk := make([]byte, 32<<10)
		limit := maxBufferedOutput / len(chunk)
		if tiny {
			chunk = []byte{1}
			limit = maxBufferedEvents
		}
		for i := 1; i <= limit; i++ {
			if _, err = s.AppendAgentEvent(&websshv1.AgentSessionEvent{Sequence: uint64(i), Event: &websshv1.AgentSessionEvent_Output{Output: chunk}}); err != nil {
				t.Fatal(err)
			}
		}
		if _, err = s.EnqueueCommand(1, Input("old-command")); err != nil {
			t.Fatal(err)
		}
		if _, err = s.AppendAgentEvent(&websshv1.AgentSessionEvent{Sequence: uint64(limit + 1), Event: &websshv1.AgentSessionEvent_Output{Output: chunk}}); !errors.Is(err, ErrOutputLimit) {
			t.Fatalf("output budget: %v", err)
		}
		if s.bufferedOutput != 0 || len(s.commands) != 0 || len(s.events) != 1 {
			t.Fatal("pressure did not release retained buffers")
		}
		event, err := s.NextEvent(context.Background(), 0)
		if err != nil || event.GetClosed().GetReason() != websshv1.CloseReason_CLOSE_REASON_FAILED {
			t.Fatalf("browser did not receive failed close: %v %v", event, err)
		}
		command, err := s.NextCommand(context.Background(), 0)
		if err != nil || command.GetCloseReason() == "" || len(command.GetInput()) != 0 {
			t.Fatalf("agent did not receive close: %v %v", command, err)
		}
		if _, err = s.AppendAgentEvent(&websshv1.AgentSessionEvent{Sequence: uint64(limit + 2), Event: &websshv1.AgentSessionEvent_Output{Output: chunk}}); !errors.Is(err, ErrClosed) {
			t.Fatalf("closed session accepted output: %v", err)
		}
	}
}

func TestAcknowledgmentReleasesEventReferencesAndCancellationWinsBacklog(t *testing.T) {
	resetSessions()
	t.Cleanup(resetSessions)
	s, _ := Create("node", "owner", "", "", nil)
	for i := uint64(1); i <= 2; i++ {
		_, err := s.AppendAgentEvent(&websshv1.AgentSessionEvent{Sequence: i, Event: &websshv1.AgentSessionEvent_Output{Output: []byte{1}}})
		if err != nil {
			t.Fatal(err)
		}
	}
	backing := s.events[:cap(s.events)]
	if _, err := s.AcknowledgeEvents(2); err != nil {
		t.Fatal(err)
	}
	if backing[0] != nil || backing[1] != nil {
		t.Fatal("ack retained event allocations")
	}
	_, _ = s.EnqueueCommand(1, Input("stale"))
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if _, err := s.NextCommand(ctx, 0); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancelled dispatch returned a queued command: %v", err)
	}
	if _, err := s.NextEvent(ctx, 0); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancelled watch ignored deadline: %v", err)
	}
}
