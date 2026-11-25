package connectapi

import (
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/komari-monitor/komari/database/metricstore"
	"github.com/komari-monitor/komari/pkg/metric"
	metricsv1 "github.com/r11234567/komari-proto/gen/go/komari/metrics/v1"
	metricsv1connect "github.com/r11234567/komari-proto/gen/go/komari/metrics/v1/metricsv1connect"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestTrafficSamplesHTTPAcknowledgesReplay(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	store, err := metric.Open(ctx, metric.SQLite(filepath.Join(dir, "metrics.db"), metric.WithSQLiteProfile(metric.SQLiteProfileDurable)))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	require.NoError(t, metricstore.EnsureBuiltinMetricDefinitions(ctx, store))
	require.NoError(t, store.EnableTelemetry(ctx, metric.TelemetryConfig{ArchiveDirectory: filepath.Join(dir, "archive")}))
	t.Cleanup(metricstore.SwapStoreForTest(store))
	telemetry := store.Telemetry()
	require.NotNil(t, telemetry)
	at := time.Now().UTC().Add(-30 * time.Second)
	// Reissuing exactly the same body must return the same receipt.
	sample := &metricsv1.SubmitTrafficSamplesRequest{
		AgentId:  testClientUUID,
		StreamId: uuid.NewString(),
		Samples: []*metricsv1.TrafficSample{{
			Sequence: 1, ObservedAt: timestamppb.New(at), BootId: "test-boot",
			Interfaces: []*metricsv1.TrafficInterfaceSample{{InterfaceId: "eth0", SentBytes: 250, ReceivedBytes: 120}},
		}},
	}
	withoutAuth := metricsv1connect.NewMetricsServiceClient(testServer.Client(), testServer.URL)
	_, err = withoutAuth.SubmitTrafficSamples(ctx, connect.NewRequest(sample))
	require.Contains(t, []connect.Code{connect.CodeUnauthenticated, connect.CodePermissionDenied}, connect.CodeOf(err))
	client := metricsv1connect.NewMetricsServiceClient(newDPoPTestClient(testAccessToken, testAgentPrivKey), testServer.URL)
	for range 2 {
		response, err := client.SubmitTrafficSamples(ctx, connect.NewRequest(sample))
		require.NoError(t, err)
		require.EqualValues(t, 1, response.Msg.AcceptedSequence)
	}
	received, err := telemetry.LatestTrafficSample(ctx, testClientUUID)
	require.NoError(t, err)
	require.Equal(t, at, received)
	pending, err := telemetry.TrafficPendingFor(ctx, testClientUUID)
	require.NoError(t, err)
	require.True(t, pending)
}
