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
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/komari-monitor/komari/web/workspace"
	filev1 "github.com/r11234567/komari-proto/gen/go/komari/file/v1"
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
	grant := workspace.FromContext(ctx)
	if grant == nil || grant.Binding.AgentID != uuid || !grant.Alive() {
		return nil, workspace.ErrLocked
	}
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
	payload, err := buildAssignmentPayload(kind, args)
	if err != nil {
		return nil, err
	}
	assignment := &filev1.FileAssignment{
		AssignmentId:   requestID,
		SystemUser:     grant.Binding.SystemUser,
		WorkspaceId:    grant.ID,
		RequestId:      requestID,
		Operation:      kind,
		TimeoutSeconds: uint32(max(timeout/time.Second, 1)),
		LeaseExpiresAt: timestamppb.New(time.Now().Add(timeout).UTC()),
	}
	setAssignmentPayload(assignment, payload)

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
	case "upload_stream":
		return filev1.FileOperation_FILE_OPERATION_UPLOAD_STREAM, true
	case "upload_commit":
		return filev1.FileOperation_FILE_OPERATION_UPLOAD_COMMIT, true
	case "upload_cancel":
		return filev1.FileOperation_FILE_OPERATION_UPLOAD_CANCEL, true
	default:
		return filev1.FileOperation_FILE_OPERATION_UNSPECIFIED, false
	}
}

func buildAssignmentPayload(kind filev1.FileOperation, args map[string]any) (any, error) {
	switch kind {
	case filev1.FileOperation_FILE_OPERATION_LIST:
		return &filev1.FileAssignment_List{
			List: &filev1.FileListPayload{Path: argString(args, "path")},
		}, nil
	case filev1.FileOperation_FILE_OPERATION_LIST_ROOTS:
		return &filev1.FileAssignment_ListRoots{
			ListRoots: &filev1.FileListRootsPayload{},
		}, nil
	case filev1.FileOperation_FILE_OPERATION_STAT:
		return &filev1.FileAssignment_Stat{
			Stat: &filev1.FileStatPayload{Path: argString(args, "path")},
		}, nil
	case filev1.FileOperation_FILE_OPERATION_CREATE:
		return &filev1.FileAssignment_Create{
			Create: &filev1.FileCreatePayload{Path: argString(args, "path"), Mode: argString(args, "mode")},
		}, nil
	case filev1.FileOperation_FILE_OPERATION_MKDIR:
		return &filev1.FileAssignment_Mkdir{
			Mkdir: &filev1.FileMkdirPayload{Path: argString(args, "path"), Mode: argString(args, "mode")},
		}, nil
	case filev1.FileOperation_FILE_OPERATION_DELETE:
		return &filev1.FileAssignment_Delete{
			Delete: &filev1.FileDeletePayload{Path: argString(args, "path"), Recursive: argBool(args, "recursive")},
		}, nil
	case filev1.FileOperation_FILE_OPERATION_MOVE:
		return &filev1.FileAssignment_Move{
			Move: &filev1.FileMovePayload{Source: argString(args, "source"), Destination: argString(args, "destination"), Overwrite: argBool(args, "overwrite")},
		}, nil
	case filev1.FileOperation_FILE_OPERATION_COPY:
		return &filev1.FileAssignment_Copy{
			Copy: &filev1.FileCopyPayload{Source: argString(args, "source"), Destination: argString(args, "destination"), Recursive: argBool(args, "recursive"), Overwrite: argBool(args, "overwrite")},
		}, nil
	case filev1.FileOperation_FILE_OPERATION_CHMOD:
		return &filev1.FileAssignment_Chmod{
			Chmod: &filev1.FileChmodPayload{Path: argString(args, "path"), Mode: argString(args, "mode")},
		}, nil
	case filev1.FileOperation_FILE_OPERATION_CHOWN:
		return &filev1.FileAssignment_Chown{
			Chown: &filev1.FileChownPayload{
				Path:  argString(args, "path"),
				Uid:   argInt32Ptr(args, "uid"),
				Gid:   argInt32Ptr(args, "gid"),
				Owner: argString(args, "owner"),
				Group: argString(args, "group"),
			},
		}, nil
	case filev1.FileOperation_FILE_OPERATION_SEARCH:
		return &filev1.FileAssignment_Search{
			Search: &filev1.FileSearchPayload{
				Path:       argString(args, "path"),
				Query:      argString(args, "query"),
				Content:    argBool(args, "content"),
				MaxResults: argUint32(args, "max_results"),
			},
		}, nil
	case filev1.FileOperation_FILE_OPERATION_DOWNLOAD_STREAM:
		return &filev1.FileAssignment_DownloadStream{
			DownloadStream: &filev1.FileDownloadStreamPayload{
				TransferId:    argString(args, "transfer_id"),
				TransferToken: argString(args, "transfer_token"),
				Path:          argString(args, "path"),
				Offset:        argInt64(args, "offset"),
				Length:        argInt64(args, "length"),
				ChunkIndex:    argInt64(args, "chunk_index"),
				FileSize:      argInt64(args, "file_size"),
				ModifiedAt:    argTimestamp(args, "modified_at"),
			},
		}, nil
	case filev1.FileOperation_FILE_OPERATION_UPLOAD_STREAM:
		return &filev1.FileAssignment_UploadStream{
			UploadStream: &filev1.FileUploadStreamPayload{
				Path:          argString(args, "path"),
				Offset:        argInt64(args, "offset"),
				ChunkIndex:    argInt64(args, "chunk_index"),
				ChunkCount:    argInt64(args, "chunk_count"),
				TotalSize:     argInt64(args, "total_size"),
				ChunkSize:     argInt64(args, "chunk_size"),
				UploadId:      argString(args, "upload_id"),
				First:         argBool(args, "first"),
				Final:         argBool(args, "final"),
				TransferId:    argString(args, "transfer_id"),
				TransferToken: argString(args, "transfer_token"),
			},
		}, nil
	case filev1.FileOperation_FILE_OPERATION_UPLOAD_COMMIT:
		return &filev1.FileAssignment_UploadCommit{
			UploadCommit: &filev1.FileUploadCommitPayload{
				Path:       argString(args, "path"),
				UploadId:   argString(args, "upload_id"),
				ChunkIndex: argInt64(args, "chunk_index"),
				ChunkCount: argInt64(args, "chunk_count"),
				TotalSize:  argInt64(args, "total_size"),
				ChunkSize:  argInt64(args, "chunk_size"),
				Offset:     argInt64(args, "offset"),
				First:      argBool(args, "first"),
				Final:      argBool(args, "final"),
			},
		}, nil
	case filev1.FileOperation_FILE_OPERATION_UPLOAD_CANCEL:
		return &filev1.FileAssignment_UploadCancel{
			UploadCancel: &filev1.FileUploadCancelPayload{
				UploadId: argString(args, "upload_id"),
				Path:     argString(args, "path"),
			},
		}, nil
	default:
		return nil, fmt.Errorf("unsupported file operation %s", kind.String())
	}
}

func setAssignmentPayload(assignment *filev1.FileAssignment, payload any) {
	switch typed := payload.(type) {
	case *filev1.FileAssignment_List:
		assignment.Payload = typed
	case *filev1.FileAssignment_ListRoots:
		assignment.Payload = typed
	case *filev1.FileAssignment_Stat:
		assignment.Payload = typed
	case *filev1.FileAssignment_Create:
		assignment.Payload = typed
	case *filev1.FileAssignment_Mkdir:
		assignment.Payload = typed
	case *filev1.FileAssignment_Delete:
		assignment.Payload = typed
	case *filev1.FileAssignment_Move:
		assignment.Payload = typed
	case *filev1.FileAssignment_Copy:
		assignment.Payload = typed
	case *filev1.FileAssignment_Chmod:
		assignment.Payload = typed
	case *filev1.FileAssignment_Chown:
		assignment.Payload = typed
	case *filev1.FileAssignment_Search:
		assignment.Payload = typed
	case *filev1.FileAssignment_DownloadStream:
		assignment.Payload = typed
	case *filev1.FileAssignment_UploadStream:
		assignment.Payload = typed
	case *filev1.FileAssignment_UploadCommit:
		assignment.Payload = typed
	case *filev1.FileAssignment_UploadCancel:
		assignment.Payload = typed
	}
}

func argString(args map[string]any, key string) string {
	if args == nil {
		return ""
	}
	s, _ := args[key].(string)
	return s
}

func argBool(args map[string]any, key string) bool {
	if args == nil {
		return false
	}
	b, _ := args[key].(bool)
	return b
}

func argInt64Ok(args map[string]any, key string) (int64, bool) {
	if args == nil {
		return 0, false
	}
	raw, exists := args[key]
	if !exists || raw == nil {
		return 0, false
	}
	switch v := raw.(type) {
	case int:
		return int64(v), true
	case int32:
		return int64(v), true
	case int64:
		return v, true
	case uint:
		if uint64(v) > math.MaxInt64 {
			return 0, false
		}
		return int64(v), true
	case uint32:
		return int64(v), true
	case uint64:
		if v > math.MaxInt64 {
			return 0, false
		}
		return int64(v), true
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return 0, false
		}
		return int64(v), true
	case string:
		parsed, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		return parsed, err == nil
	default:
		return 0, false
	}
}

func argInt64(args map[string]any, key string) int64 {
	v, _ := argInt64Ok(args, key)
	return v
}

func argInt32Ptr(args map[string]any, key string) *int32 {
	v, ok := argInt64Ok(args, key)
	if !ok || v < math.MinInt32 || v > math.MaxInt32 {
		return nil
	}
	out := int32(v)
	return &out
}

func argUint32(args map[string]any, key string) uint32 {
	v, ok := argInt64Ok(args, key)
	if !ok || v < 0 || v > math.MaxUint32 {
		return 0
	}
	return uint32(v)
}

func argTimestamp(args map[string]any, key string) *timestamppb.Timestamp {
	if args == nil {
		return nil
	}
	switch v := args[key].(type) {
	case time.Time:
		if v.IsZero() {
			return nil
		}
		return timestamppb.New(v.UTC())
	case string:
		trimmed := strings.TrimSpace(v)
		if trimmed == "" {
			return nil
		}
		if parsed, err := time.Parse(time.RFC3339Nano, trimmed); err == nil {
			return timestamppb.New(parsed.UTC())
		}
		if parsed, err := time.Parse(time.RFC3339, trimmed); err == nil {
			return timestamppb.New(parsed.UTC())
		}
		return nil
	default:
		return nil
	}
}
