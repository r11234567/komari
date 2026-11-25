package connectapi

import (
	"context"
	"errors"
	"io"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/komari-monitor/komari/database/clients"
	"github.com/komari-monitor/komari/database/enrollment"
	"github.com/komari-monitor/komari/pkg/rpc"
	agentRuntime "github.com/komari-monitor/komari/web/agent"
	"github.com/komari-monitor/komari/web/remotemanagement"
	"github.com/komari-monitor/komari/web/workspace"
	websshv1 "github.com/r11234567/komari-proto/gen/go/komari/webssh/v1"
	websshv1connect "github.com/r11234567/komari-proto/gen/go/komari/webssh/v1/websshv1connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	maxTerminalInput          = 1 << 20
	terminalHeartbeatInterval = 5 * time.Second
)

type webSSHService struct {
	websshv1connect.UnimplementedWebSSHServiceHandler
}

func (s *webSSHService) CreateSession(ctx context.Context, req *connect.Request[websshv1.CreateSessionRequest]) (*connect.Response[websshv1.CreateSessionResponse], error) {
	if req.Msg.Start == nil || strings.TrimSpace(req.Msg.Start.AgentId) == "" {
		return nil, connectError(connect.CodeInvalidArgument, errors.New("session start and agent_id are required"))
	}
	meta := rpc.MetaFromContext(ctx)
	ownerID, err := requireAdminUser(meta)
	if err != nil {
		return nil, err
	}
	_, cancel, grant, err := workspace.Authorize(ctx, req.Msg.Start.AgentId)
	if err != nil {
		return nil, connectError(connect.CodeUnauthenticated, err)
	}
	defer cancel()
	agentID := strings.TrimSpace(req.Msg.Start.AgentId)
	if _, err := clients.GetClientByUUID(agentID); err != nil {
		return nil, connectError(connect.CodeNotFound, errors.New("agent not found"))
	}
	allowed, err := clients.PrivilegedCapabilityAllowed(agentID, "webssh")
	if err != nil {
		return nil, connectError(connect.CodeInternal, err)
	}
	if !allowed {
		return nil, connectError(connect.CodePermissionDenied, errors.New("remote control is disabled for this agent"))
	}
	if !agentRuntime.IsAgentOnline(agentID) || !agentRuntime.SupportsWebSSH(agentID) {
		return nil, connectError(connect.CodeFailedPrecondition, errors.New("agent is offline or does not support Connect WebSSH"))
	}
	if err := validateTerminalSize(req.Msg.Start.Size); err != nil {
		return nil, err
	}
	session, err := remotemanagement.Create(agentID, ownerID, req.Msg.Start.Shell, req.Msg.Start.WorkingDirectory, req.Msg.Start.Size, grant)
	if err != nil {
		return nil, connectError(connect.CodeResourceExhausted, err)
	}
	return connect.NewResponse(&websshv1.CreateSessionResponse{Started: &websshv1.SessionStarted{
		SessionId: session.ID, StartedAt: timestamppb.New(session.CreatedAt),
	}}), nil
}

func (s *webSSHService) SendSessionCommand(ctx context.Context, req *connect.Request[websshv1.SendSessionCommandRequest]) (*connect.Response[websshv1.SendSessionCommandResponse], error) {
	ownerID, err := requireAdminUser(rpc.MetaFromContext(ctx))
	if err != nil {
		return nil, err
	}
	session, err := getWorkspaceSession(ctx, req.Msg.SessionId, ownerID)
	if err != nil {
		return nil, remoteSessionError(err)
	}
	var command remotemanagement.Command
	switch value := req.Msg.Command.(type) {
	case *websshv1.SendSessionCommandRequest_Input:
		if len(value.Input) == 0 || len(value.Input) > maxTerminalInput {
			return nil, connectError(connect.CodeInvalidArgument, errors.New("terminal input is empty or too large"))
		}
		command = remotemanagement.Input(value.Input)
	case *websshv1.SendSessionCommandRequest_Resize:
		if err := validateTerminalSize(value.Resize); err != nil {
			return nil, err
		}
		command = remotemanagement.Resize{Size: value.Resize}
	default:
		return nil, connectError(connect.CodeInvalidArgument, errors.New("session command is required"))
	}
	accepted, err := session.EnqueueCommand(req.Msg.Sequence, command)
	if err != nil {
		return nil, remoteSessionError(err)
	}
	return connect.NewResponse(&websshv1.SendSessionCommandResponse{AcceptedSequence: accepted}), nil
}

func (s *webSSHService) WatchSession(ctx context.Context, req *connect.Request[websshv1.WatchSessionRequest], stream *connect.ServerStream[websshv1.WatchSessionResponse]) error {
	ownerID, err := requireAdminUser(rpc.MetaFromContext(ctx))
	if err != nil {
		return err
	}
	session, err := getWorkspaceSession(ctx, req.Msg.SessionId, ownerID)
	if err != nil {
		return remoteSessionError(err)
	}
	release := session.RetainBrowser()
	defer release()
	if _, err := session.AcknowledgeEvents(req.Msg.AfterSequence); err != nil {
		return remoteSessionError(err)
	}
	sequence := req.Msg.AfterSequence
	// Flush a first frame immediately. A quiet/slow login shell must not leave
	// the browser/proxy waiting for response headers until its first PTY byte.
	if err := stream.Send(&websshv1.WatchSessionResponse{AgentAttached: session.AgentAttached()}); err != nil {
		return err
	}
	for {
		if session.Grant == nil || !session.Grant.Active() {
			return connectError(connect.CodeUnauthenticated, workspace.ErrLocked)
		}
		waitCtx, cancel := context.WithTimeout(ctx, terminalHeartbeatInterval)
		event, err := session.NextEvent(waitCtx, sequence)
		cancel()
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			if err := stream.Send(&websshv1.WatchSessionResponse{AgentAttached: session.AgentAttached()}); err != nil {
				return err
			}
			continue
		}
		if errors.Is(err, remotemanagement.ErrClosed) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := stream.Send(&websshv1.WatchSessionResponse{Event: event, AgentAttached: session.AgentAttached()}); err != nil {
			return err
		}
		sequence = event.Sequence
	}
}

func (s *webSSHService) AcknowledgeSessionEvents(ctx context.Context, req *connect.Request[websshv1.AcknowledgeSessionEventsRequest]) (*connect.Response[websshv1.AcknowledgeSessionEventsResponse], error) {
	ownerID, err := requireAdminUser(rpc.MetaFromContext(ctx))
	if err != nil {
		return nil, err
	}
	session, err := getWorkspaceSession(ctx, req.Msg.SessionId, ownerID)
	if err != nil {
		return nil, remoteSessionError(err)
	}
	accepted, err := session.AcknowledgeEvents(req.Msg.AcceptedSequence)
	if err != nil {
		return nil, remoteSessionError(err)
	}
	return connect.NewResponse(&websshv1.AcknowledgeSessionEventsResponse{AcceptedSequence: accepted}), nil
}

func (s *webSSHService) CloseSession(ctx context.Context, req *connect.Request[websshv1.CloseSessionRequest]) (*connect.Response[websshv1.CloseSessionResponse], error) {
	ownerID, err := requireAdminUser(rpc.MetaFromContext(ctx))
	if err != nil {
		return nil, err
	}
	session, err := getWorkspaceSession(ctx, req.Msg.SessionId, ownerID)
	if err != nil {
		return nil, remoteSessionError(err)
	}
	if req.Msg.Reason == "tab_closed" || req.Msg.Reason == "stream_closed" {
		// WatchSession's final consumer release starts the bounded grace period.
		// A transport reconnect during that minute may retain the same session.
		return connect.NewResponse(&websshv1.CloseSessionResponse{}), nil
	}
	return connect.NewResponse(&websshv1.CloseSessionResponse{Closed: session.Close(websshv1.CloseReason_CLOSE_REASON_CANCELLED)}), nil
}

func (s *webSSHService) LeaseSessions(ctx context.Context, req *connect.Request[websshv1.LeaseSessionsRequest], stream *connect.ServerStream[websshv1.LeaseSessionsResponse]) error {
	if req.Header().Get(workspace.IdentityHeader) != "v1" {
		return connectError(connect.CodeFailedPrecondition, errors.New("upgrade Agent: workspace OS account support is required"))
	}
	agentID, err := requireAgent(rpc.MetaFromContext(ctx), req.Msg.AgentId)
	if err != nil {
		return err
	}
	agentRuntime.MarkConnectWebSSHLease(agentID)
	// Establish the quiet lease stream before the first assignment exists and
	// keep it alive through proxy idle deadlines. Empty messages grant no work.
	if err := stream.Send(&websshv1.LeaseSessionsResponse{}); err != nil {
		return err
	}
	for {
		waitCtx, cancel := context.WithTimeout(ctx, terminalHeartbeatInterval)
		assignment, err := remotemanagement.Lease(waitCtx, agentID)
		cancel()
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			if err := stream.Send(&websshv1.LeaseSessionsResponse{}); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		allowed, e := clients.PrivilegedCapabilityAllowed(agentID, "webssh")
		if e != nil {
			return connectError(connect.CodeInternal, e)
		}
		if !allowed {
			return connectError(connect.CodePermissionDenied, errors.New("WebSSH authorization was revoked"))
		}
		expiry := assignment.GetLeaseExpiresAt().AsTime()
		assignment.Envelope, err = signAgentControl(agentID, "terminal:"+assignment.GetAssignmentId()+":"+expiry.Format(time.RFC3339Nano), expiry, assignment)
		if err != nil {
			return connectError(connect.CodeInternal, err)
		}
		if err := stream.Send(&websshv1.LeaseSessionsResponse{Assignment: assignment}); err != nil {
			return err
		}
	}
}

func (s *webSSHService) AttachSession(ctx context.Context, stream *connect.BidiStream[websshv1.AttachSessionRequest, websshv1.AttachSessionResponse]) error {
	if stream.RequestHeader().Get(workspace.IdentityHeader) != "v1" {
		return connectError(connect.CodeFailedPrecondition, errors.New("workspace OS account support is required"))
	}
	first, err := stream.Receive()
	if err != nil {
		return err
	}
	attach := first.GetAttach()
	if attach == nil {
		return connectError(connect.CodeInvalidArgument, errors.New("first attach message is required"))
	}
	if size := len(attach.GetChallenge()); size != 0 && size != 32 {
		return connectError(connect.CodeInvalidArgument, errors.New("terminal attach challenge must be 32 bytes"))
	}
	agentID, err := requireAgent(rpc.MetaFromContext(ctx), attach.AgentId)
	if err != nil {
		return err
	}
	allowed, e := clients.PrivilegedCapabilityAllowed(agentID, "webssh")
	if e != nil {
		return connectError(connect.CodeInternal, e)
	}
	if !allowed {
		return connectError(connect.CodePermissionDenied, errors.New("WebSSH authorization was revoked"))
	}
	session, err := remotemanagement.Attach(agentID, attach.AssignmentId, attach.SessionId, attach.Resumable)
	if err != nil {
		return remoteSessionError(err)
	}
	defer session.Detach()
	if !session.ValidCommandCursor(attach.AfterCommandSequence) {
		return remoteSessionError(remotemanagement.ErrSequence)
	}
	if session.Grant == nil || !session.Grant.Alive() {
		return connectError(connect.CodeUnauthenticated, workspace.ErrLocked)
	}
	receiveCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	receiveErr := make(chan error, 1)
	go func() {
		defer cancel()
		for {
			request, err := stream.Receive()
			if err != nil {
				receiveErr <- err
				return
			}
			if _, err := session.AppendAgentEvent(request.GetEvent()); err != nil {
				receiveErr <- err
				return
			}
		}
	}()
	sequence := attach.AfterCommandSequence
	var eventSequence uint64
	var frameNumber uint64
	var signingKey enrollment.SigningKey
	if len(attach.GetChallenge()) != 0 {
		signingKey, err = enrollment.ActiveSigningKey()
		if err != nil {
			return connectError(connect.CodeInternal, err)
		}
	}
	sendFrame := func(command *websshv1.AttachSessionResponse) error {
		if len(attach.GetChallenge()) == 0 {
			return stream.Send(command)
		}
		frameNumber++
		wire := proto.CloneOf(command)
		var err error
		wire.Envelope, err = signControlWithKey(signingKey, agentID, uuid.NewString(), time.Now().Add(30*time.Second), &websshv1.SessionControlFrame{
			SessionId: attach.GetSessionId(), AssignmentId: attach.GetAssignmentId(), Frame: wire,
			Challenge: attach.GetChallenge(), FrameNumber: frameNumber,
		})
		if err != nil {
			return connectError(connect.CodeInternal, err)
		}
		return stream.Send(wire)
	}
	if attach.Resumable {
		frame := session.TransportFrame(nil)
		eventSequence = frame.AcceptedEventSequence
		if err := sendFrame(frame); err != nil {
			return err
		}
	}
	for {
		var command *websshv1.AttachSessionResponse
		var err error
		if attach.Resumable {
			if !session.Grant.Alive() {
				session.Close(websshv1.CloseReason_CLOSE_REASON_DEADLINE_EXCEEDED)
				return connectError(connect.CodeUnauthenticated, workspace.ErrLocked)
			}
			wait, cancelWait := context.WithTimeout(receiveCtx, terminalHeartbeatInterval)
			command, err = session.NextResumableFrame(wait, sequence, eventSequence)
			cancelWait()
			if errors.Is(err, context.DeadlineExceeded) && receiveCtx.Err() == nil {
				command = &websshv1.AttachSessionResponse{}
				err = nil
			}
		} else {
			command, err = session.NextCommand(receiveCtx, sequence)
		}
		if err != nil {
			select {
			case receiveError := <-receiveErr:
				if errors.Is(receiveError, io.EOF) || errors.Is(receiveError, context.Canceled) {
					return nil
				}
				return receiveError
			default:
				return err
			}
		}
		if attach.Resumable {
			command = session.TransportFrame(command)
			eventSequence = command.AcceptedEventSequence
		}
		if err := sendFrame(command); err != nil {
			return err
		}
		if command.Command != nil {
			sequence = command.Sequence
		}
		if command.GetCloseReason() != "" {
			return nil
		}
	}
}

func requireAdminUser(meta *rpc.ContextMeta) (string, error) {
	if meta == nil || meta.Principal == nil || meta.Principal.UserUUID == "" || meta.Principal.Type != rpc.PrincipalUser {
		return "", connectError(connect.CodePermissionDenied, errors.New("an administrator user session is required"))
	}
	return meta.Principal.UserUUID, nil
}

func validateTerminalSize(size *websshv1.TerminalSize) error {
	if size == nil {
		return nil
	}
	if size.Rows == 0 || size.Columns == 0 || size.Rows > 1000 || size.Columns > 1000 {
		return connectError(connect.CodeInvalidArgument, errors.New("terminal size is outside the supported range"))
	}
	return nil
}

func remoteSessionError(err error) error {
	var existing *connect.Error
	if errors.As(err, &existing) {
		return err
	}
	switch {
	case errors.Is(err, remotemanagement.ErrTransportBusy):
		return connectError(connect.CodeUnavailable, err)
	case errors.Is(err, remotemanagement.ErrNotFound):
		return connectError(connect.CodeNotFound, err)
	case errors.Is(err, remotemanagement.ErrForbidden):
		return connectError(connect.CodePermissionDenied, err)
	case errors.Is(err, remotemanagement.ErrClosed), errors.Is(err, remotemanagement.ErrInvalidLease), errors.Is(err, remotemanagement.ErrSequence):
		return connectError(connect.CodeFailedPrecondition, err)
	case errors.Is(err, remotemanagement.ErrOutputLimit), errors.Is(err, remotemanagement.ErrCommandBacklog), errors.Is(err, remotemanagement.ErrSessionLimit):
		return connectError(connect.CodeResourceExhausted, err)
	default:
		return connectError(connect.CodeInternal, err)
	}
}

func getWorkspaceSession(ctx context.Context, id, owner string) (*remotemanagement.Session, error) {
	session, err := remotemanagement.GetOwned(id, owner)
	if err != nil {
		return nil, err
	}
	_, cancel, grant, err := workspace.Authorize(ctx, session.AgentID)
	defer cancel()
	if err != nil {
		return nil, connectError(connect.CodeUnauthenticated, err)
	}
	if session.Grant != grant {
		return nil, connectError(connect.CodePermissionDenied, workspace.ErrLocked)
	}
	return session, nil
}
