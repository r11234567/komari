package connectapi

import (
	"context"
	"errors"
	"io"
	"strings"

	"connectrpc.com/connect"
	"github.com/komari-monitor/komari/database/clients"
	"github.com/komari-monitor/komari/pkg/rpc"
	agentRuntime "github.com/komari-monitor/komari/web/agent"
	"github.com/komari-monitor/komari/web/remotemanagement"
	websshv1 "github.com/r11234567/komari-proto/gen/go/komari/webssh/v1"
	websshv1connect "github.com/r11234567/komari-proto/gen/go/komari/webssh/v1/websshv1connect"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	maxTerminalInput = 1 << 20
	maxFileChunk     = 384 << 10
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
	if err := verifyConnectTwoFactor(meta, req.Msg.Start.TwoFactor); err != nil {
		return nil, err
	}
	agentID := strings.TrimSpace(req.Msg.Start.AgentId)
	if _, err := clients.GetClientByUUID(agentID); err != nil {
		return nil, connectError(connect.CodeNotFound, errors.New("agent not found"))
	}
	allowed, err := clients.RemoteControlAllowed(agentID)
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
	session, err := remotemanagement.Create(agentID, ownerID, req.Msg.Start.Shell, req.Msg.Start.WorkingDirectory, req.Msg.Start.Size)
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
	session, err := remotemanagement.GetOwned(req.Msg.SessionId, ownerID)
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
	case *websshv1.SendSessionCommandRequest_File:
		if err := validateFileCommand(value.File); err != nil {
			return nil, err
		}
		command = remotemanagement.File{Command: value.File}
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
	session, err := remotemanagement.GetOwned(req.Msg.SessionId, ownerID)
	if err != nil {
		return remoteSessionError(err)
	}
	if _, err := session.AcknowledgeEvents(req.Msg.AfterSequence); err != nil {
		return remoteSessionError(err)
	}
	sequence := req.Msg.AfterSequence
	for {
		event, err := session.NextEvent(ctx, sequence)
		if errors.Is(err, remotemanagement.ErrClosed) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := stream.Send(&websshv1.WatchSessionResponse{Event: event}); err != nil {
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
	session, err := remotemanagement.GetOwned(req.Msg.SessionId, ownerID)
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
	session, err := remotemanagement.GetOwned(req.Msg.SessionId, ownerID)
	if err != nil {
		return nil, remoteSessionError(err)
	}
	return connect.NewResponse(&websshv1.CloseSessionResponse{Closed: session.Close(websshv1.CloseReason_CLOSE_REASON_CANCELLED)}), nil
}

func (s *webSSHService) LeaseSessions(ctx context.Context, req *connect.Request[websshv1.LeaseSessionsRequest], stream *connect.ServerStream[websshv1.LeaseSessionsResponse]) error {
	agentID, err := requireAgent(rpc.MetaFromContext(ctx), req.Msg.AgentId)
	if err != nil {
		return err
	}
	agentRuntime.MarkConnectWebSSHLease(agentID)
	for {
		assignment, err := remotemanagement.Lease(ctx, agentID)
		if err != nil {
			return err
		}
		if err := stream.Send(&websshv1.LeaseSessionsResponse{Assignment: assignment}); err != nil {
			return err
		}
	}
}

func (s *webSSHService) AttachSession(ctx context.Context, stream *connect.BidiStream[websshv1.AttachSessionRequest, websshv1.AttachSessionResponse]) error {
	first, err := stream.Receive()
	if err != nil {
		return err
	}
	attach := first.GetAttach()
	if attach == nil {
		return connectError(connect.CodeInvalidArgument, errors.New("first attach message is required"))
	}
	agentID, err := requireAgent(rpc.MetaFromContext(ctx), attach.AgentId)
	if err != nil {
		return err
	}
	session, err := remotemanagement.Attach(agentID, attach.AssignmentId, attach.SessionId)
	if err != nil {
		return remoteSessionError(err)
	}
	defer session.Detach()
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
	for {
		command, err := session.NextCommand(receiveCtx, sequence)
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
		if err := stream.Send(command); err != nil {
			return err
		}
		sequence = command.Sequence
		if command.GetCloseReason() != "" {
			return nil
		}
	}
}

func (s *webSSHService) OpenSession(ctx context.Context, stream *connect.BidiStream[websshv1.OpenSessionRequest, websshv1.OpenSessionResponse]) error {
	first, err := stream.Receive()
	if err != nil {
		return err
	}
	start := first.GetStart()
	if start == nil || strings.TrimSpace(start.AgentId) == "" {
		return connectError(connect.CodeInvalidArgument, errors.New("first message must contain a session start"))
	}
	meta := rpc.MetaFromContext(ctx)
	ownerID, err := requireAdminUser(meta)
	if err != nil {
		return err
	}
	if err := verifyConnectTwoFactor(meta, start.TwoFactor); err != nil {
		return connectError(connect.CodeUnauthenticated, err)
	}
	agentID := strings.TrimSpace(start.AgentId)
	if _, err := clients.GetClientByUUID(agentID); err != nil {
		return connectError(connect.CodeNotFound, errors.New("agent not found"))
	}
	allowed, err := clients.RemoteControlAllowed(agentID)
	if err != nil {
		return connectError(connect.CodeInternal, err)
	}
	if !allowed {
		return connectError(connect.CodePermissionDenied, errors.New("remote control is disabled for this agent"))
	}
	if !agentRuntime.IsAgentOnline(agentID) || !agentRuntime.SupportsWebSSH(agentID) {
		return connectError(connect.CodeFailedPrecondition, errors.New("agent is offline or does not support Connect WebSSH"))
	}
	if err := validateTerminalSize(start.Size); err != nil {
		return err
	}
	session, err := remotemanagement.Create(agentID, ownerID, start.Shell, start.WorkingDirectory, start.Size)
	if err != nil {
		return remoteSessionError(err)
	}
	defer session.Close(websshv1.CloseReason_CLOSE_REASON_CANCELLED)
	if err := stream.Send(&websshv1.OpenSessionResponse{Event: &websshv1.OpenSessionResponse_Started{Started: &websshv1.SessionStarted{
		SessionId: session.ID, StartedAt: timestamppb.New(session.CreatedAt),
	}}}); err != nil {
		return err
	}
	receiveCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	receiveErr := make(chan error, 1)
	go func() {
		var commandSequence uint64
		for {
			request, err := stream.Receive()
			if err != nil {
				receiveErr <- err
				cancel()
				return
			}
			switch value := request.Event.(type) {
			case *websshv1.OpenSessionRequest_Input:
				if value.Input.SessionId != session.ID || len(value.Input.Data) == 0 || len(value.Input.Data) > maxTerminalInput {
					receiveErr <- errors.New("invalid terminal input")
					cancel()
					return
				}
				_, err = session.EnqueueCommand(value.Input.Sequence, remotemanagement.Input(value.Input.Data))
				if value.Input.Sequence > commandSequence {
					commandSequence = value.Input.Sequence
				}
			case *websshv1.OpenSessionRequest_Resize:
				if value.Resize.SessionId != session.ID {
					err = errors.New("invalid terminal resize session")
					break
				}
				if err = validateTerminalSize(value.Resize.Size); err == nil {
					commandSequence++
					_, err = session.EnqueueCommand(commandSequence, remotemanagement.Resize{Size: value.Resize.Size})
				}
			default:
				err = errors.New("unsupported native session event")
			}
			if err != nil {
				receiveErr <- err
				cancel()
				return
			}
		}
	}()
	var outputSequence uint64
	for {
		event, err := session.NextEvent(receiveCtx, outputSequence)
		if err != nil {
			select {
			case receiveError := <-receiveErr:
				if errors.Is(receiveError, io.EOF) || errors.Is(receiveError, context.Canceled) {
					return nil
				}
				return receiveError
			default:
				if errors.Is(err, remotemanagement.ErrClosed) || errors.Is(err, context.Canceled) {
					return nil
				}
				return err
			}
		}
		outputSequence = event.Sequence
		var response *websshv1.OpenSessionResponse
		switch value := event.Event.(type) {
		case *websshv1.SessionEvent_Output:
			response = &websshv1.OpenSessionResponse{Event: &websshv1.OpenSessionResponse_Output{Output: &websshv1.TerminalOutput{SessionId: session.ID, Sequence: event.Sequence, Data: value.Output}}}
		case *websshv1.SessionEvent_Closed:
			response = &websshv1.OpenSessionResponse{Event: &websshv1.OpenSessionResponse_Closed{Closed: value.Closed}}
		default:
			continue
		}
		if err := stream.Send(response); err != nil {
			return err
		}
		if event.GetClosed() != nil {
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

func validateFileCommand(command *websshv1.FileCommand) error {
	if command == nil || command.Operation == websshv1.FileOperation_FILE_OPERATION_UNSPECIFIED || strings.TrimSpace(command.RequestId) == "" {
		return connectError(connect.CodeInvalidArgument, errors.New("typed file operation and request_id are required"))
	}
	if len(command.Data) > maxFileChunk {
		return connectError(connect.CodeResourceExhausted, errors.New("file chunk is too large"))
	}
	return nil
}

func remoteSessionError(err error) error {
	switch {
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
