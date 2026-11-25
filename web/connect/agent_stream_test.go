package connectapi

import (
	"context"
	"net/http"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/komari-monitor/komari/database/enrollment"
	metricsv1 "github.com/r11234567/komari-proto/gen/go/komari/metrics/v1"
	metricsv1connect "github.com/r11234567/komari-proto/gen/go/komari/metrics/v1/metricsv1connect"
	"github.com/stretchr/testify/require"
)

func TestRevocationInterruptsInactiveBidiStreamOverHTTP2(t *testing.T) {
	server := entryBoundaryServer(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	for _, mode := range []string{"connect", "grpc"} {
		t.Run(mode, func(t *testing.T) {
			open := func() (string, *connect.BidiStreamForClient[metricsv1.StreamMetricsRequest, metricsv1.StreamMetricsResponse]) {
				t.Helper()
				id, token, key := dpopTestAgent(t)
				transport := &testDPoPTransport{base: server.Client().Transport, token: token, privKey: key}
				var options []connect.ClientOption
				if mode == "grpc" {
					options = append(options, connect.WithGRPC())
				}
				client := metricsv1connect.NewMetricsServiceClient(&http.Client{Transport: transport}, server.URL, options...)
				stream := client.StreamMetrics(ctx)
				t.Cleanup(func() { _ = stream.CloseResponse(); _ = stream.CloseRequest() })
				return id, stream
			}
			report := func(id string, sequence uint64) *metricsv1.StreamMetricsRequest {
				return &metricsv1.StreamMetricsRequest{Batch: &metricsv1.SubmitMetricsRequest{
					AgentId: id, Sequence: sequence, Points: completeMetricPoints(),
				}}
			}
			id, revoked := open()
			otherID, other := open()
			require.NoError(t, revoked.Send(report(id, 1)))
			accepted, err := revoked.Receive()
			require.NoError(t, err)
			require.EqualValues(t, 1, accepted.GetAcceptedSequence())
			require.NoError(t, other.Send(report(otherID, 1)))
			_, err = other.Receive()
			require.NoError(t, err)

			// Leave both request bodies open. The server is blocked on Receive.
			require.NoError(t, enrollment.Revoke(id, "operator revoked the test credential"))
			received := make(chan error, 1)
			go func() {
				_, err := revoked.Receive()
				received <- err
			}()
			select {
			case err := <-received:
				require.Error(t, err)
			case <-time.After(2 * time.Second):
				t.Fatal("revoked bidi stream stayed open")
			}
			require.NoError(t, other.Send(report(otherID, 2)))
			accepted, err = other.Receive()
			require.NoError(t, err)
			require.EqualValues(t, 2, accepted.GetAcceptedSequence())
		})
	}
}
