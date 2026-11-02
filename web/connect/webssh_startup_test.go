package connectapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
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

func TestWebSSHColdStartupFramesPrecedePTYOutputAndDoNotAdvanceCursor(t *testing.T) {
	runWebSSHColdStartup(t, false)
}

func TestWebSSHColdStartupNginx(t *testing.T) {
	if os.Getenv("KOMARI_NGINX_COMPONENT_TEST") != "1" {
		t.Skip("requires isolated Nginx component runner")
	}
	runWebSSHColdStartup(t, true)
}

func runWebSSHColdStartup(t *testing.T, throughNginx bool) {
	t.Helper()
	ctx, user, login := actionFixture(t)
	agent, _, err := clients.CreateClient()
	require.NoError(t, err)
	db := dbcore.GetDBInstance()
	require.NoError(t, db.Create(&models.ClientPrivilegedRevision{Client: agent, Revision: 1, Config: `{"remote_control_enabled":true,"webssh_enabled":true}`}).Error)
	require.NoError(t, db.Create(&models.ClientPrivilegedState{Client: agent, ProtocolVersion: 2, AppliedRevision: 1, DesiredRevision: 1}).Error)
	token, snapshot, err := workspace.Default.Create(workspace.Binding{UserID: user, Session: login, AgentID: agent, SystemUser: "alice"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = workspace.Default.Lock(token, user, login) })
	session, err := remotemanagement.Create(agent, user, "", "", nil, snapshot.Grant)
	require.NoError(t, err)
	t.Cleanup(func() { session.Close(websshv1.CloseReason_CLOSE_REASON_CANCELLED) })
	_, handler := websshv1connect.NewWebSSHServiceHandler(&webSSHService{}, connect.WithInterceptors(newPolicyInterceptor()))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		meta := rpc.MetaFromContext(ctx)
		if strings.HasSuffix(r.URL.Path, "/LeaseSessions") {
			meta = &rpc.ContextMeta{Principal: rpc.NewAgentPrincipal(agent), ClientUUID: agent}
		}
		bound := workspace.WithToken(rpc.NewContextWithMeta(r.Context(), meta), token)
		handler.ServeHTTP(w, r.WithContext(bound))
	}))
	defer server.Close()
	client := websshv1connect.NewWebSSHServiceClient(server.Client(), server.URL)
	if throughNginx {
		nginx, err := exec.LookPath("nginx")
		require.NoError(t, err)
		endpoint := startWorkbenchNginx(t, nginx, strings.TrimPrefix(server.URL, "http://"), "proxy_read_timeout 1s;")
		client = websshv1connect.NewWebSSHServiceClient(workbenchHTTP2Client(t), endpoint)
	}
	streamCtx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	stream, err := client.WatchSession(streamCtx, connect.NewRequest(&websshv1.WatchSessionRequest{SessionId: session.ID}))
	require.NoError(t, err)
	defer stream.Close()
	start := time.Now()
	require.True(t, stream.Receive(), stream.Err())
	require.Nil(t, stream.Msg().Event)
	require.False(t, stream.Msg().AgentAttached)
	require.Less(t, time.Since(start), 2*time.Second, "cold response waited for a shell which has not started")
	// Quiet lease streams must also establish promptly before real work arrives.
	leaseRequest := connect.NewRequest(&websshv1.LeaseSessionsRequest{AgentId: agent})
	leaseRequest.Header().Set(workspace.IdentityHeader, "v1")
	lease, err := client.LeaseSessions(streamCtx, leaseRequest)
	require.NoError(t, err)
	defer lease.Close()
	require.True(t, lease.Receive(), lease.Err())
	require.Nil(t, lease.Msg().Assignment)
	require.True(t, lease.Receive(), lease.Err())
	assignment := lease.Msg().Assignment
	require.NotNil(t, assignment)
	_, err = remotemanagement.Attach(agent, assignment.AssignmentId, assignment.SessionId)
	require.NoError(t, err)
	// The process exists but deliberately has no prompt/output yet. A status
	// frame must report precisely that state, without inventing terminal bytes.
	require.True(t, stream.Receive(), stream.Err())
	require.Nil(t, stream.Msg().Event)
	require.True(t, stream.Msg().AgentAttached)
	_, err = session.AppendAgentEvent(&websshv1.AgentSessionEvent{Sequence: 1, Event: &websshv1.AgentSessionEvent_Output{Output: []byte("prompt$ ")}})
	require.NoError(t, err)
	require.True(t, stream.Receive(), stream.Err())
	require.Equal(t, uint64(1), stream.Msg().Event.Sequence)
	require.Equal(t, "prompt$ ", string(stream.Msg().Event.GetOutput()))
	accepted, err := session.AcknowledgeEvents(1)
	require.NoError(t, err)
	require.Equal(t, uint64(1), accepted)
	require.NoError(t, workspace.Default.Lock(token, user, login))
	lockedAt := time.Now()
	for stream.Receive() {
		if stream.Msg().Event != nil {
			require.Empty(t, stream.Msg().Event.GetOutput(), "revoked grant emitted more output")
		}
	}
	// Either a clean close or an authentication error is a valid end. What
	// matters is prompt closure, before the long request deadline, without
	// heartbeat activity extending the revoked authorization.
	require.Less(t, time.Since(lockedAt), time.Second)
	require.NoError(t, streamCtx.Err(), "watch ended only because its outer deadline elapsed")
}
