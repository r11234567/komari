// Package filemanager correlates panel file requests with agent responses.
//
// The control plane only carries metadata. File contents use the one-shot
// HTTP transfer endpoints, matching the upstream Komari file manager design.
package filemanager

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	filev1 "github.com/r11234567/komari-proto/gen/go/komari/file/v1"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const defaultTimeout = 30 * time.Second

var (
	// ErrOffline means the agent has no active file channel.
	ErrOffline = errors.New("agent is not connected")
	// ErrTimeout means the agent did not answer in time.
	ErrTimeout = errors.New("file operation timed out")
	// ErrUnknownToken means a transfer token is unknown or expired.
	ErrUnknownToken = errors.New("unknown or expired file operation")
	// ErrUnsupported means the agent did not advertise file management.
	ErrUnsupported = errors.New("agent does not support file operations")
)

// Dispatcher delivers one assignment to an attached agent. It returns false
// when the agent has no file channel.
type Dispatcher func(agentID string, assignment *filev1.FileAssignment) bool

var (
	dispatchMu sync.RWMutex
	dispatch   Dispatcher

	pendingMu sync.Mutex
	pending   = make(map[string]pendingCall)
)

type pendingCall struct {
	uuid     string
	response chan *filev1.AgentFileEvent
}

// CallOptions bounds one control operation.
type CallOptions struct {
	// Timeout defaults to 30 seconds.
	Timeout time.Duration
}

// SetDispatcher installs the transport used to reach agents. Passing nil
// disables dispatch; tests use it to isolate correlation.
func SetDispatcher(next Dispatcher) {
	dispatchMu.Lock()
	dispatch = next
	dispatchMu.Unlock()
}

// Call dispatches a metadata-only filesystem control operation and waits for
// the matching FileEvent. Binary data must use the HTTP transfer endpoints.
func Call(ctx context.Context, uuid string, operation string, args map[string]any, options ...CallOptions) (*filev1.AgentFileEvent, error) {
	kind, ok := operationFromString(operation)
	if !ok {
		return nil, fmt.Errorf("unsupported file operation %q", operation)
	}
	dispatchMu.RLock()
	send := dispatch
	dispatchMu.RUnlock()
	if send == nil {
		return nil, ErrOffline
	}
	timeout := defaultTimeout
	if len(options) > 0 && options[0].Timeout > 0 {
		timeout = options[0].Timeout
	}
	requestID, err := newRequestID()
	if err != nil {
		return nil, err
	}
	argumentsMap := make(map[string]any, len(args)+1)
	for key, value := range args {
		argumentsMap[key] = value
	}
	// The wire enum has no commit/cancel members; carry the upstream operation
	// name so the agent can dispatch the finalize/cancel handlers.
	argumentsMap["_op"] = operation
	arguments, err := structpb.NewStruct(argumentsMap)
	if err != nil {
		return nil, fmt.Errorf("encode file arguments: %w", err)
	}
	assignment := &filev1.FileAssignment{
		AssignmentId:   requestID,
		RequestId:      requestID,
		Operation:      kind,
		Arguments:      arguments,
		TimeoutSeconds: uint32(max(timeout/time.Second, 1)),
		LeaseExpiresAt: timestamppb.New(time.Now().Add(timeout).UTC()),
	}

	responses := make(chan *filev1.AgentFileEvent, 1)
	pendingMu.Lock()
	pending[requestID] = pendingCall{uuid: uuid, response: responses}
	pendingMu.Unlock()
	defer removePending(requestID)

	if !send(uuid, assignment) {
		return nil, ErrOffline
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case event := <-responses:
		if !event.GetSuccess() {
			if event.GetError() == "" {
				return nil, errors.New("file operation failed")
			}
			return nil, errors.New(event.GetError())
		}
		return event, nil
	case <-timer.C:
		return nil, ErrTimeout
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Resolve delivers one agent event to the caller waiting for its request.
// Events from another agent never resolve a pending request.
func Resolve(agentID string, event *filev1.AgentFileEvent) bool {
	if event == nil || event.GetRequestId() == "" {
		return false
	}
	pendingMu.Lock()
	wait, ok := pending[event.GetRequestId()]
	if ok && wait.uuid != agentID {
		pendingMu.Unlock()
		return false
	}
	delete(pending, event.GetRequestId())
	pendingMu.Unlock()
	if !ok {
		return false
	}
	wait.response <- event
	return true
}

func removePending(requestID string) {
	pendingMu.Lock()
	delete(pending, requestID)
	pendingMu.Unlock()
}

func newRequestID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", fmt.Errorf("generate file request id: %w", err)
	}
	return hex.EncodeToString(id[:]), nil
}

// operationFromString maps the upstream wire names to the typed contract.
func operationFromString(value string) (filev1.FileOperation, bool) {
	switch strings.TrimSpace(value) {
	case "list":
		return filev1.FileOperation_FILE_OPERATION_LIST, true
	case "list_roots":
		return filev1.FileOperation_FILE_OPERATION_LIST_ROOTS, true
	case "stat":
		return filev1.FileOperation_FILE_OPERATION_STAT, true
	case "create":
		return filev1.FileOperation_FILE_OPERATION_CREATE, true
	case "mkdir":
		return filev1.FileOperation_FILE_OPERATION_MKDIR, true
	case "delete":
		return filev1.FileOperation_FILE_OPERATION_DELETE, true
	case "move":
		return filev1.FileOperation_FILE_OPERATION_MOVE, true
	case "copy":
		return filev1.FileOperation_FILE_OPERATION_COPY, true
	case "chmod":
		return filev1.FileOperation_FILE_OPERATION_CHMOD, true
	case "chown":
		return filev1.FileOperation_FILE_OPERATION_CHOWN, true
	case "search":
		return filev1.FileOperation_FILE_OPERATION_SEARCH, true
	case "download_stream":
		return filev1.FileOperation_FILE_OPERATION_DOWNLOAD_STREAM, true
	case "upload_stream", "upload_commit", "upload_cancel":
		return filev1.FileOperation_FILE_OPERATION_UPLOAD_STREAM, true
	default:
		return filev1.FileOperation_FILE_OPERATION_UNSPECIFIED, false
	}
}
