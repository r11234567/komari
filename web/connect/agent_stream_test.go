package connectapi

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/komari-monitor/komari/database/enrollment"
	"github.com/komari-monitor/komari/web/api"
	metricsv1 "github.com/r11234567/komari-proto/gen/go/komari/metrics/v1"
	metricsv1connect "github.com/r11234567/komari-proto/gen/go/komari/metrics/v1/metricsv1connect"
	"github.com/stretchr/testify/require"
)

func TestAgentTransferRequiresDPoPBeforeReadingFileBody(t *testing.T) {
	id, token, key := dpopTestAgent(t)
	publicKey := key.Public().(ed25519.PublicKey)
	engine := gin.New()
	engine.Use(api.APIAuthenticationMiddleware(), api.IdentityMiddleware())
	var hits atomic.Int32
	engine.POST(api.AgentTransferRoute, GuardAgentTransfer, func(c *gin.Context) {
		hits.Add(1)
		count, err := io.Copy(io.Discard, c.Request.Body)
		if err != nil || count != maxDPoPBodyBytes+1 {
			t.Errorf("transfer body size = %d, error = %v", count, err)
		}
		c.Status(http.StatusNoContent)
	})
	server := httptest.NewUnstartedServer(engine)
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()

	endpoint := server.URL + "/api/clients/transfer/test"
	call := func(target, auth, proof string, body io.Reader) int {
		t.Helper()
		request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, target, body)
		require.NoError(t, err)
		request.Header.Set("Authorization", auth)
		request.Header.Set("DPoP", proof)
		response, err := server.Client().Do(request)
		require.NoError(t, err)
		defer response.Body.Close()
		require.Equal(t, 2, response.ProtoMajor)
		return response.StatusCode
	}
	auth := "DPoP " + token
	nonce := currentDPoPNonce(id, time.Now())
	proof := makeRFC9449Proof(t, key, publicKey, http.MethodPost, endpoint, token, nonce, uuid.NewString(), nil, "stream")
	require.Equal(t, http.StatusUnauthorized, call(endpoint, "Bearer "+token, proof, nil))
	require.Equal(t, http.StatusUnauthorized, call(endpoint, auth, "", nil))
	require.Equal(t, http.StatusUnauthorized, call(endpoint+"-different", auth, proof, nil))
	require.Equal(t, http.StatusBadRequest, call(endpoint+"?transfer_token=fixture", auth, proof, nil))
	require.Zero(t, hits.Load())

	body := make([]byte, maxDPoPBodyBytes+1)
	require.Equal(t, http.StatusNoContent, call(endpoint, auth, proof, bytes.NewReader(body)))
	require.Equal(t, http.StatusUnauthorized, call(endpoint, auth, proof, nil))
	require.EqualValues(t, 1, hits.Load())
}

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
