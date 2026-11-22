package connectapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/komari-monitor/komari/database/clients"
	"github.com/komari-monitor/komari/pkg/rpc"
	"github.com/komari-monitor/komari/web/workspace"
	agentv1 "github.com/r11234567/komari-proto/gen/go/komari/agent/v1"
	agentv1connect "github.com/r11234567/komari-proto/gen/go/komari/agent/v1/agentv1connect"
	configv1 "github.com/r11234567/komari-proto/gen/go/komari/config/v1"
	configv1connect "github.com/r11234567/komari-proto/gen/go/komari/config/v1/configv1connect"
	filev1 "github.com/r11234567/komari-proto/gen/go/komari/file/v1"
	filev1connect "github.com/r11234567/komari-proto/gen/go/komari/file/v1/filev1connect"
	"github.com/stretchr/testify/require"
)

func withShortStreamHeartbeat(t *testing.T, interval time.Duration) {
	t.Helper()
	prev := streamHeartbeatInterval
	streamHeartbeatInterval = interval
	t.Cleanup(func() {
		streamHeartbeatInterval = prev
	})
}

func wrapAgentMux(t *testing.T, agentID, path string, handler http.Handler) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle(path, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := rpc.NewContextWithMeta(r.Context(), &rpc.ContextMeta{
			Principal:  rpc.NewAgentPrincipal(agentID),
			Permission: rpc.RoleClient,
			ClientUUID: agentID,
		})
		handler.ServeHTTP(w, r.WithContext(ctx))
	}))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestStreamHeartbeatsEmittedWhenIdle(t *testing.T) {
	withShortStreamHeartbeat(t, 25*time.Millisecond)

	t.Run("LeaseFileOperations", func(t *testing.T) {
		agentID := "hb-file-agent"
		path, handler := filev1connect.NewFileServiceHandler(newFileService(), connect.WithInterceptors(newPolicyInterceptor()))
		srv := wrapAgentMux(t, agentID, path, handler)
		client := filev1connect.NewFileServiceClient(srv.Client(), srv.URL)

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		req := connect.NewRequest(&filev1.LeaseFileOperationsRequest{AgentId: agentID})
		req.Header().Set(workspace.IdentityHeader, "v1")
		stream, err := client.LeaseFileOperations(ctx, req)
		require.NoError(t, err)
		defer stream.Close()

		require.True(t, stream.Receive(), "expected heartbeat frame: %v", stream.Err())
		require.True(t, stream.Msg().GetHeartbeat())
		require.Nil(t, stream.Msg().GetAssignment())
	})

	t.Run("SubscribeEvents", func(t *testing.T) {
		agentID := "hb-event-agent"
		path, handler := agentv1connect.NewAgentEventServiceHandler(&agentEventService{}, connect.WithInterceptors(newPolicyInterceptor()))
		srv := wrapAgentMux(t, agentID, path, handler)
		client := agentv1connect.NewAgentEventServiceClient(srv.Client(), srv.URL)

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		stream, err := client.SubscribeEvents(ctx, connect.NewRequest(&agentv1.SubscribeEventsRequest{AgentId: agentID}))
		require.NoError(t, err)
		defer stream.Close()

		require.True(t, stream.Receive(), "expected heartbeat frame: %v", stream.Err())
		require.True(t, stream.Msg().GetHeartbeat())
		require.Nil(t, stream.Msg().GetEvent())
	})

	t.Run("WatchDesiredConfig", func(t *testing.T) {
		agentID, _, err := clients.CreateClient()
		require.NoError(t, err)
		path, handler := configv1connect.NewConfigServiceHandler(&configService{}, connect.WithInterceptors(newPolicyInterceptor()))
		srv := wrapAgentMux(t, agentID, path, handler)
		client := configv1connect.NewConfigServiceClient(srv.Client(), srv.URL)

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		stream, err := client.WatchDesiredConfig(ctx, connect.NewRequest(&configv1.WatchDesiredConfigRequest{AgentId: agentID}))
		require.NoError(t, err)
		defer stream.Close()

		require.True(t, stream.Receive(), "expected heartbeat frame: %v", stream.Err())
		require.True(t, stream.Msg().GetHeartbeat())
		require.Nil(t, stream.Msg().GetDesired())
	})

	t.Run("WatchPrivilegedDeliveryNonRescueAndRescue", func(t *testing.T) {
		id, token, _, mux := rescueWatchFixture(t)
		srv := httptest.NewServer(mux)
		defer srv.Close()
		client := configv1connect.NewPrivilegedDeliveryServiceClient(srv.Client(), srv.URL)

		// Non-rescue stream emits bare Heartbeat when no update is queued.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		stream, err := client.WatchPrivilegedDelivery(ctx, connect.NewRequest(&configv1.WatchPrivilegedDeliveryRequest{
			AgentId:         id,
			ProtocolVersion: 2,
			AfterSequence:   ^uint64(0) >> 1,
		}))
		require.NoError(t, err)
		require.True(t, stream.Receive(), "expected non-rescue heartbeat frame: %v", stream.Err())
		require.True(t, stream.Msg().GetHeartbeat())
		require.Nil(t, stream.Msg().GetRevision())
		_ = stream.Close()

		// Rescue stream accepts Authorization: DPoP <token> re-authentication and emits signed revisions.
		rctx, rcancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer rcancel()
		rreq := connect.NewRequest(&configv1.WatchPrivilegedDeliveryRequest{AgentId: id, ProtocolVersion: 2})
		rreq.Header().Set(rescueWatchHeader, "v1")
		rreq.Header().Set("Authorization", "DPoP "+token)
		rstream, err := client.WatchPrivilegedDelivery(rctx, rreq)
		require.NoError(t, err)
		defer rstream.Close()

		require.True(t, rstream.Receive(), "expected signed revision in rescue mode: %v", rstream.Err())
		require.False(t, rstream.Msg().GetHeartbeat())
		require.NotNil(t, rstream.Msg().GetRevision())
	})
}
