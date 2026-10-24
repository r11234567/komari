package connectapi

import (
	"context"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"
	filev1 "github.com/r11234567/komari-proto/gen/go/komari/file/v1"
)

func TestFileStoreDispatchAndTake(t *testing.T) {
	store := &fileStore{
		agents:      make(map[string]chan *filev1.FileAssignment),
		assignments: make(map[string]*fileAssignment),
	}
	assignment := &filev1.FileAssignment{AssignmentId: "a-1", RequestId: "r-1"}
	if store.dispatch("agent-1", assignment) {
		t.Fatal("offline agent must not accept an assignment")
	}

	channel := make(chan *filev1.FileAssignment, 1)
	store.registerAgent("agent-1", channel)
	if !store.dispatch("agent-1", assignment) {
		t.Fatal("connected agent must accept an assignment")
	}
	select {
	case queued := <-channel:
		if queued.GetAssignmentId() != "a-1" {
			t.Fatalf("queued assignment = %q", queued.GetAssignmentId())
		}
	case <-time.After(time.Second):
		t.Fatal("assignment was not queued")
	}
	if got := store.takeAssignment("agent-1", "a-1"); got == nil || got.GetRequestId() != "r-1" {
		t.Fatalf("take assignment = %+v", got)
	}
	store.finishAssignment("a-1")
	if got := store.takeAssignment("agent-1", "a-1"); got != nil {
		t.Fatal("finished assignment still resolvable")
	}
}

func TestFileStoreRejectsWrongAgent(t *testing.T) {
	store := &fileStore{
		agents:      map[string]chan *filev1.FileAssignment{"agent-1": make(chan *filev1.FileAssignment, 1)},
		assignments: make(map[string]*fileAssignment),
	}
	if !store.dispatch("agent-1", &filev1.FileAssignment{AssignmentId: "a-2"}) {
		t.Fatal("dispatch failed")
	}
	if got := store.takeAssignment("agent-2", "a-2"); got != nil {
		t.Fatal("another agent resolved the assignment")
	}
}

func TestPathParent(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"/var/log", "/var"},
		{"/", ""},
		{`C:\Users`, "C:"},
		{"/tmp/", ""},
	}
	for _, testCase := range cases {
		if got := pathParent(testCase.input); got != testCase.want {
			t.Errorf("pathParent(%q) = %q, want %q", testCase.input, got, testCase.want)
		}
	}
}

func TestLookupFileError(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantCode connect.Code
	}{
		{
			name:     "Windows file not found",
			err:      errors.New("GetFileAttributesEx E:\\missing.txt: The system cannot find the file specified."),
			wantCode: connect.CodeNotFound,
		},
		{
			name:     "Windows path not found",
			err:      errors.New("open E:\\missing\\dir: The system cannot find the path specified."),
			wantCode: connect.CodeNotFound,
		},
		{
			name:     "Linux file not found",
			err:      errors.New("stat /missing: no such file or directory"),
			wantCode: connect.CodeNotFound,
		},
		{
			name:     "Permission denied",
			err:      errors.New("open /root/secret: permission denied"),
			wantCode: connect.CodePermissionDenied,
		},
		{
			name:     "Operation not permitted",
			err:      errors.New("operation not permitted"),
			wantCode: connect.CodePermissionDenied,
		},
		{
			name:     "Windows chown unsupported",
			err:      errors.New("user and group ownership are only supported on Unix systems"),
			wantCode: connect.CodeUnimplemented,
		},
		{
			name:     "Generic unsupported",
			err:      errors.New("unsupported file operation: foo"),
			wantCode: connect.CodeUnimplemented,
		},
		{
			name:     "Required path argument",
			err:      errors.New("file path is required"),
			wantCode: connect.CodeInvalidArgument,
		},
		{
			name:     "Refuse delete root",
			err:      errors.New("refusing to delete a filesystem root"),
			wantCode: connect.CodeInvalidArgument,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := lookupFileError(tc.err)
			var connectErr *connect.Error
			if !errors.As(got, &connectErr) {
				t.Fatalf("expected connect.Error, got %T: %v", got, got)
			}
			if connectErr.Code() != tc.wantCode {
				t.Errorf("code = %v, want %v", connectErr.Code(), tc.wantCode)
			}
		})
	}
}

func TestVerifyConnectTwoFactorRejectionCode(t *testing.T) {
	// 1. Missing context meta / unauthenticated
	err := verifyConnectTwoFactor(nil, nil)
	var connectErr *connect.Error
	if !errors.As(err, &connectErr) {
		t.Fatalf("expected *connect.Error, got %T: %v", err, err)
	}
	if connectErr.Code() != connect.CodeUnauthenticated {
		t.Errorf("expected CodeUnauthenticated for nil meta, got %v", connectErr.Code())
	}
}

func TestFileServiceMutatingOperationsGatedBy2FA(t *testing.T) {
	service := newFileService()
	ctx := context.Background()

	// CreateEntry
	_, err := service.CreateEntry(ctx, connect.NewRequest(&filev1.CreateEntryRequest{
		AgentId: "agent-1",
		Path:    "/tmp/test.txt",
	}))
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("CreateEntry without 2FA code = %v, want CodeUnauthenticated", connect.CodeOf(err))
	}

	// DeleteEntry
	_, err = service.DeleteEntry(ctx, connect.NewRequest(&filev1.DeleteEntryRequest{
		AgentId: "agent-1",
		Path:    "/tmp/test.txt",
	}))
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("DeleteEntry without 2FA code = %v, want CodeUnauthenticated", connect.CodeOf(err))
	}

	// MoveEntry
	_, err = service.MoveEntry(ctx, connect.NewRequest(&filev1.MoveEntryRequest{
		AgentId:     "agent-1",
		Source:      "/tmp/test.txt",
		Destination: "/tmp/test2.txt",
	}))
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("MoveEntry without 2FA code = %v, want CodeUnauthenticated", connect.CodeOf(err))
	}

	// CopyEntry
	_, err = service.CopyEntry(ctx, connect.NewRequest(&filev1.CopyEntryRequest{
		AgentId:     "agent-1",
		Source:      "/tmp/test.txt",
		Destination: "/tmp/test2.txt",
	}))
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("CopyEntry without 2FA code = %v, want CodeUnauthenticated", connect.CodeOf(err))
	}

	// ChmodEntry
	_, err = service.ChmodEntry(ctx, connect.NewRequest(&filev1.ChmodEntryRequest{
		AgentId: "agent-1",
		Path:    "/tmp/test.txt",
		Mode:    "0644",
	}))
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("ChmodEntry without 2FA code = %v, want CodeUnauthenticated", connect.CodeOf(err))
	}

	// ChownEntry
	_, err = service.ChownEntry(ctx, connect.NewRequest(&filev1.ChownEntryRequest{
		AgentId: "agent-1",
		Path:    "/tmp/test.txt",
		Owner:   "root",
	}))
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("ChownEntry without 2FA code = %v, want CodeUnauthenticated", connect.CodeOf(err))
	}
}

func TestFileServiceReadOperationsReturnUnavailableWhenOffline(t *testing.T) {
	service := newFileService()
	ctx := context.Background()

	// ListDirectory on offline agent
	_, err := service.ListDirectory(ctx, connect.NewRequest(&filev1.ListDirectoryRequest{
		AgentId: "offline-agent",
		Path:    "/tmp",
	}))
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Errorf("ListDirectory on offline agent code = %v, want CodeUnavailable", connect.CodeOf(err))
	}

	// StatFile on offline agent
	_, err = service.StatFile(ctx, connect.NewRequest(&filev1.StatFileRequest{
		AgentId: "offline-agent",
		Path:    "/tmp/foo",
	}))
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Errorf("StatFile on offline agent code = %v, want CodeUnavailable", connect.CodeOf(err))
	}

	// SearchFiles on offline agent
	_, err = service.SearchFiles(ctx, connect.NewRequest(&filev1.SearchFilesRequest{
		AgentId: "offline-agent",
		Path:    "/tmp",
		Query:   "foo",
	}))
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Errorf("SearchFiles on offline agent code = %v, want CodeUnavailable", connect.CodeOf(err))
	}
}
