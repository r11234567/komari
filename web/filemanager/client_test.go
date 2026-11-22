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

func TestCallBuildsTypedPayloadsWithoutLegacyArguments(t *testing.T) {
	cases := []struct {
		name      string
		op        string
		args      map[string]any
		wantOp    filev1.FileOperation
		checkFunc func(t *testing.T, a *filev1.FileAssignment)
	}{
		{
			name:   "list",
			op:     "list",
			args:   map[string]any{"path": "/etc"},
			wantOp: filev1.FileOperation_FILE_OPERATION_LIST,
			checkFunc: func(t *testing.T, a *filev1.FileAssignment) {
				if p := a.GetList(); p == nil || p.GetPath() != "/etc" {
					t.Fatalf("unexpected list payload: %+v", p)
				}
			},
		},
		{
			name: "download_stream",
			op:   "download_stream",
			args: map[string]any{
				"transfer_id":    "tid-1",
				"transfer_token": "tok-1",
				"path":           "/var/log/syslog",
				"offset":         int64(128),
				"length":         int64(4096),
				"chunk_index":    int64(2),
				"file_size":      int64(8192),
				"modified_at":    "2026-11-22T06:00:00Z",
			},
			wantOp: filev1.FileOperation_FILE_OPERATION_DOWNLOAD_STREAM,
			checkFunc: func(t *testing.T, a *filev1.FileAssignment) {
				p := a.GetDownloadStream()
				if p == nil || p.GetTransferId() != "tid-1" || p.GetOffset() != 128 || p.GetLength() != 4096 || p.GetModifiedAt() == nil {
					t.Fatalf("unexpected download_stream payload: %+v", p)
				}
			},
		},
		{
			name: "upload_stream",
			op:   "upload_stream",
			args: map[string]any{
				"path":           "/tmp/a.bin",
				"offset":         int64(0),
				"chunk_index":    int64(0),
				"chunk_count":    int64(3),
				"total_size":     int64(100),
				"chunk_size":     int64(40),
				"upload_id":      "up-1",
				"first":          true,
				"final":          false,
				"transfer_id":    "tid-2",
				"transfer_token": "tok-2",
			},
			wantOp: filev1.FileOperation_FILE_OPERATION_UPLOAD_STREAM,
			checkFunc: func(t *testing.T, a *filev1.FileAssignment) {
				p := a.GetUploadStream()
				if p == nil || p.GetUploadId() != "up-1" || !p.GetFirst() || p.GetFinal() || p.GetTransferToken() != "tok-2" {
					t.Fatalf("unexpected upload_stream payload: %+v", p)
				}
			},
		},
		{
			name: "upload_commit",
			op:   "upload_commit",
			args: map[string]any{
				"path":        "/tmp/a.bin",
				"upload_id":   "up-1",
				"chunk_index": int64(3),
				"chunk_count": int64(3),
				"total_size":  int64(100),
				"chunk_size":  int64(40),
				"offset":      int64(100),
				"final":       true,
			},
			wantOp: filev1.FileOperation_FILE_OPERATION_UPLOAD_COMMIT,
			checkFunc: func(t *testing.T, a *filev1.FileAssignment) {
				p := a.GetUploadCommit()
				if p == nil || p.GetUploadId() != "up-1" || !p.GetFinal() || p.GetTotalSize() != 100 {
					t.Fatalf("unexpected upload_commit payload: %+v", p)
				}
			},
		},
		{
			name:   "upload_cancel",
			op:     "upload_cancel",
			args:   map[string]any{"upload_id": "up-1", "path": "/tmp/a.bin"},
			wantOp: filev1.FileOperation_FILE_OPERATION_UPLOAD_CANCEL,
			checkFunc: func(t *testing.T, a *filev1.FileAssignment) {
				p := a.GetUploadCancel()
				if p == nil || p.GetUploadId() != "up-1" || p.GetPath() != "/tmp/a.bin" {
					t.Fatalf("unexpected upload_cancel payload: %+v", p)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got *filev1.FileAssignment
			withDispatcher(t, func(agentID string, assignment *filev1.FileAssignment) bool {
				got = assignment
				go Resolve(agentID, &filev1.AgentFileEvent{RequestId: assignment.GetRequestId(), Success: true})
				return true
			})
			_, err := Call(testWorkspaceContext(t, "agent-1"), "agent-1", tc.op, tc.args, CallOptions{Timeout: time.Second})
			if err != nil {
				t.Fatalf("call %s: %v", tc.op, err)
			}
			if got == nil {
				t.Fatal("missing assignment")
			}
			if got.GetArguments() != nil {
				t.Fatalf("expected nil deprecated Arguments, got %+v", got.GetArguments())
			}
			if got.GetOperation() != tc.wantOp {
				t.Fatalf("operation = %v, want %v", got.GetOperation(), tc.wantOp)
			}
			tc.checkFunc(t, got)
		})
	}
}

func TestLegacyFileAssignmentPreservesOldUploadDiscriminators(t *testing.T) {
	for _, operation := range []string{"upload_commit", "upload_cancel"} {
		t.Run(operation, func(t *testing.T) {
			withDispatcher(t, func(agent string, assignment *filev1.FileAssignment) bool {
				legacy, err := LegacyAssignment(agent, assignment)
				if err != nil {
					t.Fatal(err)
				}
				if legacy.GetOperation() != filev1.FileOperation_FILE_OPERATION_UPLOAD_STREAM ||
					legacy.GetArguments().GetFields()["_op"].GetStringValue() != operation || legacy.GetPayload() != nil {
					t.Fatalf("legacy assignment did not match the old schema: %v", legacy)
				}
				if assignment.GetPayload() == nil || assignment.GetArguments() != nil {
					t.Fatal("legacy projection mutated the typed assignment")
				}
				return Resolve(agent, &filev1.AgentFileEvent{RequestId: assignment.GetRequestId(), Success: true})
			})
			args := map[string]any{"path": "/tmp/test", "upload_id": "old-agent", "total_size": int64(8), "chunk_size": int64(4), "chunk_count": int64(2)}
			if _, err := Call(testWorkspaceContext(t, "agent-1"), "agent-1", operation, args); err != nil {
				t.Fatal(err)
			}
		})
	}
}
