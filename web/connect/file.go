package connectapi

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/komari-monitor/komari/database/auditlog"
	"github.com/komari-monitor/komari/pkg/rpc"
	"github.com/komari-monitor/komari/web/filemanager"
	filev1 "github.com/r11234567/komari-proto/gen/go/komari/file/v1"
	filev1connect "github.com/r11234567/komari-proto/gen/go/komari/file/v1/filev1connect"
)

const (
	fileControlTimeout = 30 * time.Second
	fileSearchTimeout  = 90 * time.Second
	fileLeaseTTL       = 2 * time.Minute
)

type fileService struct {
	filev1connect.UnimplementedFileServiceHandler
	store *fileStore
}

type fileStore struct {
	mu          sync.Mutex
	agents      map[string]chan *filev1.FileAssignment
	assignments map[string]*fileAssignment
}

type fileAssignment struct {
	agentID    string
	assignment *filev1.FileAssignment
	expiresAt  time.Time
}

func newFileService() *fileService {
	store := &fileStore{
		agents:      make(map[string]chan *filev1.FileAssignment),
		assignments: make(map[string]*fileAssignment),
	}
	filemanager.SetDispatcher(store.dispatch)
	return &fileService{store: store}
}

func (s *fileStore) dispatch(agentID string, assignment *filev1.FileAssignment) bool {
	if agentID == "" || assignment == nil || assignment.GetAssignmentId() == "" {
		return false
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, pending := range s.assignments {
		if now.After(pending.expiresAt) {
			delete(s.assignments, id)
		}
	}
	channel, ok := s.agents[agentID]
	if !ok {
		return false
	}
	s.assignments[assignment.GetAssignmentId()] = &fileAssignment{
		agentID:    agentID,
		assignment: assignment,
		expiresAt:  now.Add(fileLeaseTTL),
	}
	select {
	case channel <- assignment:
		return true
	default:
		delete(s.assignments, assignment.GetAssignmentId())
		return false
	}
}

func (s *fileStore) registerAgent(agentID string, channel chan *filev1.FileAssignment) {
	s.mu.Lock()
	s.agents[agentID] = channel
	s.mu.Unlock()
}

func (s *fileStore) unregisterAgent(agentID string, channel chan *filev1.FileAssignment) {
	s.mu.Lock()
	if current := s.agents[agentID]; current == channel {
		delete(s.agents, agentID)
	}
	s.mu.Unlock()
}

func (s *fileStore) takeAssignment(agentID, assignmentID string) *filev1.FileAssignment {
	s.mu.Lock()
	defer s.mu.Unlock()
	pending, ok := s.assignments[assignmentID]
	if !ok || pending.agentID != agentID || time.Now().After(pending.expiresAt) {
		delete(s.assignments, assignmentID)
		return nil
	}
	return pending.assignment
}

func (s *fileStore) finishAssignment(assignmentID string) {
	s.mu.Lock()
	delete(s.assignments, assignmentID)
	s.mu.Unlock()
}

func (s *fileService) call(ctx context.Context, agentID, operation string, args map[string]any, timeout time.Duration) (*filev1.AgentFileEvent, error) {
	if strings.TrimSpace(agentID) == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("agent_id is required"))
	}
	event, err := filemanager.Call(ctx, agentID, operation, args, filemanager.CallOptions{Timeout: timeout})
	if err != nil {
		return nil, lookupFileError(err)
	}
	return event, nil
}

func lookupFileError(err error) error {
	switch {
	case errors.Is(err, filemanager.ErrOffline):
		return connect.NewError(connect.CodeUnavailable, errors.New("agent is offline or has no file channel"))
	case errors.Is(err, filemanager.ErrTimeout):
		return connect.NewError(connect.CodeDeadlineExceeded, err)
	case errors.Is(err, filemanager.ErrUnsupported):
		return connect.NewError(connect.CodeFailedPrecondition, err)
	case errors.Is(err, context.Canceled):
		return connect.NewError(connect.CodeCanceled, err)
	case errors.Is(err, context.DeadlineExceeded):
		return connect.NewError(connect.CodeDeadlineExceeded, err)
	}
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "permission denied"), strings.Contains(message, "operation not permitted"):
		return connect.NewError(connect.CodePermissionDenied, err)
	case strings.Contains(message, "no such file"), strings.Contains(message, "not found"):
		return connect.NewError(connect.CodeNotFound, err)
	case strings.Contains(message, "unsupported"):
		return connect.NewError(connect.CodeUnimplemented, err)
	default:
		return connect.NewError(connect.CodeInternal, err)
	}
}

func fileEntryPath(entries []*filev1.FileEntry, fallback string) string {
	if len(entries) > 0 && entries[0].GetPath() != "" {
		return entries[0].GetPath()
	}
	return fallback
}

func pathParent(value string) string {
	cleaned := strings.TrimRight(value, "/\\")
	if cleaned == "" || cleaned == "." {
		return ""
	}
	parent := path.Dir(strings.ReplaceAll(cleaned, "\\", "/"))
	if parent == "." || parent == "/" {
		return ""
	}
	return parent
}

func (s *fileService) audit(ctx context.Context, operation, agentID, target string) {
	meta := rpc.MetaFromContext(ctx)
	if meta == nil {
		return
	}
	auditlog.Log(meta.RemoteIP, meta.UserUUID, fmt.Sprintf("file %s: %s:%s", operation, agentID, target), "warn")
}

func (s *fileService) ListDirectory(ctx context.Context, req *connect.Request[filev1.ListDirectoryRequest]) (*connect.Response[filev1.ListDirectoryResponse], error) {
	event, err := s.call(ctx, req.Msg.GetAgentId(), "list", map[string]any{"path": req.Msg.GetPath()}, fileControlTimeout)
	if err != nil {
		return nil, err
	}
	listed := event.GetPath()
	if listed == "" {
		listed = req.Msg.GetPath()
	}
	return connect.NewResponse(&filev1.ListDirectoryResponse{
		Listing: &filev1.DirectoryListing{
			Entries:   event.GetEntries(),
			Path:      listed,
			Parent:    pathParent(listed),
			Truncated: event.GetTruncated(),
		},
	}), nil
}

func (s *fileService) ListRoots(ctx context.Context, req *connect.Request[filev1.ListRootsRequest]) (*connect.Response[filev1.ListRootsResponse], error) {
	event, err := s.call(ctx, req.Msg.GetAgentId(), "list_roots", map[string]any{"path": ""}, fileControlTimeout)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&filev1.ListRootsResponse{Roots: event.GetEntries()}), nil
}

func (s *fileService) StatFile(ctx context.Context, req *connect.Request[filev1.StatFileRequest]) (*connect.Response[filev1.StatFileResponse], error) {
	event, err := s.call(ctx, req.Msg.GetAgentId(), "stat", map[string]any{"path": req.Msg.GetPath()}, fileControlTimeout)
	if err != nil {
		return nil, err
	}
	entries := event.GetEntries()
	if len(entries) == 0 {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("path metadata is missing"))
	}
	return connect.NewResponse(&filev1.StatFileResponse{Entry: entries[0]}), nil
}

func (s *fileService) CreateEntry(ctx context.Context, req *connect.Request[filev1.CreateEntryRequest]) (*connect.Response[filev1.CreateEntryResponse], error) {
	meta := rpc.MetaFromContext(ctx)
	if err := verifyConnectTwoFactor(meta, req.Msg.GetTwoFactor()); err != nil {
		return nil, err
	}
	operation := "create"
	if req.Msg.GetKind() == filev1.EntryKind_ENTRY_KIND_DIRECTORY {
		operation = "mkdir"
	}
	event, err := s.call(ctx, req.Msg.GetAgentId(), operation, map[string]any{
		"path": req.Msg.GetPath(),
		"mode": req.Msg.GetMode(),
	}, fileControlTimeout)
	if err != nil {
		return nil, err
	}
	entries := event.GetEntries()
	var entry *filev1.FileEntry
	if len(entries) > 0 {
		entry = entries[0]
	}
	s.audit(ctx, operation, req.Msg.GetAgentId(), req.Msg.GetPath())
	return connect.NewResponse(&filev1.CreateEntryResponse{Entry: entry}), nil
}

func (s *fileService) DeleteEntry(ctx context.Context, req *connect.Request[filev1.DeleteEntryRequest]) (*connect.Response[filev1.DeleteEntryResponse], error) {
	meta := rpc.MetaFromContext(ctx)
	if err := verifyConnectTwoFactor(meta, req.Msg.GetTwoFactor()); err != nil {
		return nil, err
	}
	event, err := s.call(ctx, req.Msg.GetAgentId(), "delete", map[string]any{
		"path":      req.Msg.GetPath(),
		"recursive": req.Msg.GetRecursive(),
	}, fileControlTimeout)
	if err != nil {
		return nil, err
	}
	s.audit(ctx, "delete", req.Msg.GetAgentId(), req.Msg.GetPath())
	return connect.NewResponse(&filev1.DeleteEntryResponse{Path: fileEntryPath(event.GetEntries(), req.Msg.GetPath()), Deleted: event.GetSuccess()}), nil
}

func (s *fileService) MoveEntry(ctx context.Context, req *connect.Request[filev1.MoveEntryRequest]) (*connect.Response[filev1.MoveEntryResponse], error) {
	meta := rpc.MetaFromContext(ctx)
	if err := verifyConnectTwoFactor(meta, req.Msg.GetTwoFactor()); err != nil {
		return nil, err
	}
	event, err := s.call(ctx, req.Msg.GetAgentId(), "move", map[string]any{
		"source":      req.Msg.GetSource(),
		"destination": req.Msg.GetDestination(),
		"overwrite":   req.Msg.GetOverwrite(),
	}, fileControlTimeout)
	if err != nil {
		return nil, err
	}
	s.audit(ctx, "move", req.Msg.GetAgentId(), req.Msg.GetSource()+" -> "+req.Msg.GetDestination())
	return connect.NewResponse(&filev1.MoveEntryResponse{Entry: fileEntry(event.GetEntries())}), nil
}

func (s *fileService) CopyEntry(ctx context.Context, req *connect.Request[filev1.CopyEntryRequest]) (*connect.Response[filev1.CopyEntryResponse], error) {
	meta := rpc.MetaFromContext(ctx)
	if err := verifyConnectTwoFactor(meta, req.Msg.GetTwoFactor()); err != nil {
		return nil, err
	}
	event, err := s.call(ctx, req.Msg.GetAgentId(), "copy", map[string]any{
		"source":      req.Msg.GetSource(),
		"destination": req.Msg.GetDestination(),
		"recursive":   req.Msg.GetRecursive(),
		"overwrite":   req.Msg.GetOverwrite(),
	}, fileControlTimeout)
	if err != nil {
		return nil, err
	}
	s.audit(ctx, "copy", req.Msg.GetAgentId(), req.Msg.GetSource()+" -> "+req.Msg.GetDestination())
	return connect.NewResponse(&filev1.CopyEntryResponse{Entry: fileEntry(event.GetEntries())}), nil
}

func (s *fileService) ChmodEntry(ctx context.Context, req *connect.Request[filev1.ChmodEntryRequest]) (*connect.Response[filev1.ChmodEntryResponse], error) {
	meta := rpc.MetaFromContext(ctx)
	if err := verifyConnectTwoFactor(meta, req.Msg.GetTwoFactor()); err != nil {
		return nil, err
	}
	event, err := s.call(ctx, req.Msg.GetAgentId(), "chmod", map[string]any{
		"path": req.Msg.GetPath(),
		"mode": req.Msg.GetMode(),
	}, fileControlTimeout)
	if err != nil {
		return nil, err
	}
	s.audit(ctx, "chmod", req.Msg.GetAgentId(), req.Msg.GetPath())
	return connect.NewResponse(&filev1.ChmodEntryResponse{Entry: fileEntry(event.GetEntries())}), nil
}

func (s *fileService) ChownEntry(ctx context.Context, req *connect.Request[filev1.ChownEntryRequest]) (*connect.Response[filev1.ChownEntryResponse], error) {
	meta := rpc.MetaFromContext(ctx)
	if err := verifyConnectTwoFactor(meta, req.Msg.GetTwoFactor()); err != nil {
		return nil, err
	}
	args := map[string]any{"path": req.Msg.GetPath(), "owner": req.Msg.GetOwner(), "group": req.Msg.GetGroup()}
	if req.Msg.Uid != nil {
		args["uid"] = float64(req.Msg.GetUid())
	}
	if req.Msg.Gid != nil {
		args["gid"] = float64(req.Msg.GetGid())
	}
	event, err := s.call(ctx, req.Msg.GetAgentId(), "chown", args, fileControlTimeout)
	if err != nil {
		return nil, err
	}
	s.audit(ctx, "chown", req.Msg.GetAgentId(), req.Msg.GetPath())
	return connect.NewResponse(&filev1.ChownEntryResponse{Entry: fileEntry(event.GetEntries())}), nil
}

func (s *fileService) SearchFiles(ctx context.Context, req *connect.Request[filev1.SearchFilesRequest]) (*connect.Response[filev1.SearchFilesResponse], error) {
	event, err := s.call(ctx, req.Msg.GetAgentId(), "search", map[string]any{
		"path":        req.Msg.GetPath(),
		"query":       req.Msg.GetQuery(),
		"content":     req.Msg.GetContent(),
		"max_results": float64(req.Msg.GetMaxResults()),
	}, fileSearchTimeout)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&filev1.SearchFilesResponse{
		Matches: event.GetMatches(),
		Limited: event.GetLimited(),
		Path:    fileEntryPath(nil, req.Msg.GetPath()),
	}), nil
}

func (s *fileService) LeaseFileOperations(ctx context.Context, req *connect.Request[filev1.LeaseFileOperationsRequest], stream *connect.ServerStream[filev1.LeaseFileOperationsResponse]) error {
	agentID := strings.TrimSpace(req.Msg.GetAgentId())
	if agentID == "" {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("agent_id is required"))
	}
	channel := make(chan *filev1.FileAssignment, 32)
	s.store.registerAgent(agentID, channel)
	defer s.store.unregisterAgent(agentID, channel)
	for {
		select {
		case <-ctx.Done():
			return nil
		case assignment := <-channel:
			if err := stream.Send(&filev1.LeaseFileOperationsResponse{Assignment: assignment}); err != nil {
				return err
			}
		}
	}
}

func (s *fileService) AttachFileOperations(ctx context.Context, stream *connect.BidiStream[filev1.AttachFileOperationsRequest, filev1.AttachFileOperationsResponse]) error {
	first, err := stream.Receive()
	if err != nil {
		return err
	}
	attach := first.GetAttach()
	if attach == nil || strings.TrimSpace(attach.GetAgentId()) == "" || strings.TrimSpace(attach.GetAssignmentId()) == "" {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("agent and assignment are required"))
	}
	assignment := s.store.takeAssignment(attach.GetAgentId(), attach.GetAssignmentId())
	if assignment == nil {
		return connect.NewError(connect.CodeNotFound, errors.New("file assignment is unknown or expired"))
	}
	if err := stream.Send(&filev1.AttachFileOperationsResponse{
		Command: &filev1.AttachFileOperationsResponse_Assignment{Assignment: assignment},
	}); err != nil {
		return err
	}
	event, err := stream.Receive()
	if err != nil {
		return err
	}
	result := event.GetEvent()
	if result == nil {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("an agent file event is required"))
	}
	s.store.finishAssignment(assignment.GetAssignmentId())
	filemanager.Resolve(attach.GetAgentId(), result)
	return nil
}

func fileEntry(entries []*filev1.FileEntry) *filev1.FileEntry {
	if len(entries) == 0 {
		return nil
	}
	return entries[0]
}
