package filemanager

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/komari-monitor/komari/web/workspace"
	filev1 "github.com/r11234567/komari-proto/gen/go/komari/file/v1"
)

func withDispatcher(t *testing.T, dispatcher Dispatcher) {
	t.Helper()
	SetDispatcher(dispatcher)
	t.Cleanup(func() { SetDispatcher(nil) })
}

func TestCallResolvesMatchingEvent(t *testing.T) {
	withDispatcher(t, func(agentID string, assignment *filev1.FileAssignment) bool {
		go func() {
			event := &filev1.AgentFileEvent{
				RequestId: assignment.GetRequestId(),
				Path:      "/",
				Success:   true,
			}
			if !Resolve(agentID, event) {
				t.Errorf("event was not resolved")
			}
		}()
		return true
	})

	event, err := Call(testWorkspaceContext(t, "agent-1"), "agent-1", "list", map[string]any{"path": "/"}, CallOptions{Timeout: time.Second})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if event.GetPath() != "/" {
		t.Fatalf("path = %q", event.GetPath())
	}
}

func TestCallReturnsAgentError(t *testing.T) {
	withDispatcher(t, func(agentID string, assignment *filev1.FileAssignment) bool {
		go Resolve(agentID, &filev1.AgentFileEvent{
			RequestId: assignment.GetRequestId(),
			Success:   false,
			Error:     "permission denied",
		})
		return true
	})

	_, err := Call(testWorkspaceContext(t, "agent-1"), "agent-1", "stat", map[string]any{"path": "/root"}, CallOptions{Timeout: time.Second})
	if err == nil || err.Error() != "permission denied" {
		t.Fatalf("error = %v", err)
	}
}

func TestCallOfflineAndTimeout(t *testing.T) {
	withDispatcher(t, nil)
	if _, err := Call(testWorkspaceContext(t, "agent-1"), "agent-1", "list", nil); !errors.Is(err, ErrOffline) {
		t.Fatalf("offline error = %v", err)
	}

	withDispatcher(t, func(string, *filev1.FileAssignment) bool { return true })
	if _, err := Call(testWorkspaceContext(t, "agent-2"), "agent-2", "list", nil, CallOptions{Timeout: 20 * time.Millisecond}); !errors.Is(err, ErrTimeout) {
		t.Fatalf("timeout error = %v", err)
	}
}

func TestResolveRejectsWrongAgent(t *testing.T) {
	delivered := make(chan bool, 1)
	withDispatcher(t, func(agentID string, assignment *filev1.FileAssignment) bool {
		delivered <- Resolve("agent-other", &filev1.AgentFileEvent{RequestId: assignment.GetRequestId(), Success: true})
		Resolve(agentID, &filev1.AgentFileEvent{RequestId: assignment.GetRequestId(), Success: true})
		return true
	})
	if _, err := Call(testWorkspaceContext(t, "agent-1"), "agent-1", "list", nil, CallOptions{Timeout: time.Second}); err != nil {
		t.Fatalf("call: %v", err)
	}
	if <-delivered {
		t.Fatal("event from another agent resolved the request")
	}
}

func testWorkspaceContext(t *testing.T, agent string) context.Context {
	t.Helper()
	store := workspace.NewStore(time.Now, func(workspace.Binding) bool { return true })
	_, snapshot, err := store.Create(workspace.Binding{UserID: "synthetic-user", Session: "synthetic-session", AgentID: agent, SystemUser: "synthetic-account"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := workspace.Bind(context.Background(), snapshot.Grant)
	t.Cleanup(cancel)
	return ctx
}
func TestCallRejectsUnscopedAndWrongAgent(t *testing.T) {
	for _, ctx := range []context.Context{context.Background(), testWorkspaceContext(t, "other-agent")} {
		if _, err := Call(ctx, "agent-1", "list", nil); !errors.Is(err, workspace.ErrLocked) {
			t.Fatalf("scope bypass: %v", err)
		}
	}
}
