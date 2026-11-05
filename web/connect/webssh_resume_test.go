package connectapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/komari-monitor/komari/database/clients"
	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/pkg/rpc"
	"github.com/komari-monitor/komari/web/remotemanagement"
	"github.com/komari-monitor/komari/web/workspace"
	websshv1 "github.com/r11234567/komari-proto/gen/go/komari/webssh/v1"
	websshv1connect "github.com/r11234567/komari-proto/gen/go/komari/webssh/v1/websshv1connect"
	"github.com/stretchr/testify/require"
)

// Real HTTP/2 and production Connect handler/store, with an isolated account
// and a synthetic Agent wire peer. Real PTY preservation is tested in Agent.
func TestWebSSHTransportResumesCheckpointsOverHTTP2(t *testing.T) {
	_, user, login := actionFixture(t)
	agent, _, err := clients.CreateClient()
	require.NoError(t, err)
	db := dbcore.GetDBInstance()
	require.NoError(t, db.Create(&models.ClientPrivilegedRevision{Client: agent, Revision: 1, Config: `{"remote_control_enabled":true,"webssh_enabled":true}`}).Error)
	require.NoError(t, db.Create(&models.ClientPrivilegedState{Client: agent, ProtocolVersion: 2, AppliedRevision: 1, DesiredRevision: 1}).Error)
	token, grant, err := workspace.Default.Create(workspace.Binding{UserID: user, Session: login, AgentID: agent, SystemUser: "alice"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = workspace.Default.Lock(token, user, login) })
	session, err := remotemanagement.Create(agent, user, "", "", nil, grant.Grant)
	require.NoError(t, err)
	t.Cleanup(func() { session.Close(websshv1.CloseReason_CLOSE_REASON_CANCELLED) })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	assignment, err := remotemanagement.Lease(ctx, agent)
	require.NoError(t, err)
	_, handler := websshv1connect.NewWebSSHServiceHandler(&webSSHService{}, connect.WithInterceptors(newPolicyInterceptor()))
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		meta := &rpc.ContextMeta{Principal: rpc.NewAgentPrincipal(agent), ClientUUID: agent}
		handler.ServeHTTP(w, r.WithContext(rpc.NewContextWithMeta(r.Context(), meta)))
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	client := websshv1connect.NewWebSSHServiceClient(server.Client(), server.URL)
	attach := func(after uint64) (*connect.BidiStreamForClient[websshv1.AttachSessionRequest, websshv1.AttachSessionResponse], context.CancelFunc) {
		connectionCtx, stop := context.WithCancel(ctx)
		stream := client.AttachSession(connectionCtx)
		stream.RequestHeader().Set(workspace.IdentityHeader, "v1")
		require.NoError(t, stream.Send(&websshv1.AttachSessionRequest{Message: &websshv1.AttachSessionRequest_Attach{Attach: &websshv1.AgentSessionAttach{
			AgentId: agent, AssignmentId: assignment.AssignmentId, SessionId: session.ID, AfterCommandSequence: after, Resumable: true,
		}}}))
		return stream, stop
	}
	first, stop := attach(0)
	defer stop()
	frame, err := first.Receive()
	require.NoError(t, err)
	require.EqualValues(t, 0, frame.AcceptedEventSequence)
	require.False(t, frame.ResumeUntil.AsTime().After(grant.IdleExpiresAt))
	output := func(sequence uint64, text string) *websshv1.AttachSessionRequest {
		return &websshv1.AttachSessionRequest{Message: &websshv1.AttachSessionRequest_Event{Event: &websshv1.AgentSessionEvent{
			Sequence: sequence, AcceptedCommandSequence: sequence - 1, Event: &websshv1.AgentSessionEvent_Output{Output: []byte(text)},
		}}}
	}
	require.NoError(t, first.Send(output(1, "before")))
	frame, err = first.Receive()
	require.NoError(t, err)
	require.EqualValues(t, 1, frame.AcceptedEventSequence)
	_, err = session.EnqueueCommand(1, remotemanagement.Input("once\n"))
	require.NoError(t, err)
	frame, err = first.Receive()
	require.NoError(t, err)
	require.EqualValues(t, 1, frame.Sequence)
	require.Equal(t, "once\n", string(frame.GetInput()))
	// Lose the command ACK and interrupt this exact HTTP/2 request.
	stop()
	_ = first.CloseResponse()
	_ = first.CloseRequest()
	require.Eventually(t, func() bool { return !session.AgentAttached() }, time.Second, time.Millisecond)
	second, stopSecond := attach(1)
	defer stopSecond()
	frame, err = second.Receive()
	require.NoError(t, err)
	require.EqualValues(t, 1, frame.AcceptedEventSequence, "backend lost accepted output")
	require.Nil(t, frame.Command)
	// Ambiguous output can be replayed safely. Only the new output is appended.
	replayed := output(1, "before")
	replayed.GetEvent().AcceptedCommandSequence = 1
	require.NoError(t, second.Send(replayed))
	require.NoError(t, second.Send(output(2, "after")))
	for {
		frame, err = second.Receive()
		require.NoError(t, err)
		require.Nil(t, frame.Command, "already-applied input was replayed")
		if frame.AcceptedEventSequence == 2 {
			break
		}
	}
	before, err := session.NextEvent(ctx, 0)
	require.NoError(t, err)
	after, err := session.NextEvent(ctx, before.Sequence)
	require.NoError(t, err)
	require.Equal(t, "before", string(before.GetOutput()))
	require.Equal(t, "after", string(after.GetOutput()))
	require.EqualValues(t, 2, after.Sequence)
	require.NoError(t, workspace.Default.Lock(token, user, login))
	for {
		frame, err = second.Receive()
		if err != nil || frame.GetCloseReason() != "" {
			break
		}
	}
	require.NoError(t, ctx.Err(), "revocation waited for the outer request timeout")
}
