package connectapi

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/komari-monitor/komari/database/trafficevidence"
	metrics "github.com/r11234567/komari-proto/gen/go/komari/metrics/v1"
	metricsconnect "github.com/r11234567/komari-proto/gen/go/komari/metrics/v1/metricsv1connect"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestTrafficEvidenceOnlyBoundAgentCanUpload(t *testing.T) {
	ctx := t.Context()
	closeEvidence, err := trafficevidence.Start(filepath.Join(t.TempDir(), "store.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, closeEvidence(context.Background())) })
	end := time.Now().UTC().Add(-time.Minute)
	event := &metrics.TrafficEvidenceIncident{
		Id: uuid.NewString(), Reason: "fast_unexplained_ingress", InterfaceName: "eth0", Sampler: "nfpcapd",
		Start: timestamppb.New(end.Add(-2 * time.Minute)), End: timestamppb.New(end),
		TriggeredAt: timestamppb.New(end.Add(-time.Minute)), Complete: true,
		Observed: &metrics.TrafficEvidenceBytes{RxBytes: 1000},
		IngressSources: []*metrics.TrafficEvidencePeer{
			{Ip: "192.0.2.2", Bytes: 700}, {Ip: "2001:db8::1", Bytes: 300},
		},
	}
	body := &metrics.SubmitTrafficEvidenceRequest{
		AgentId: testClientUUID, Record: &metrics.SubmitTrafficEvidenceRequest_Incident{Incident: event},
	}
	anon := metricsconnect.NewMetricsServiceClient(testServer.Client(), testServer.URL)
	_, err = anon.SubmitTrafficEvidence(ctx, connect.NewRequest(body))
	require.Contains(t, []connect.Code{connect.CodeUnauthenticated, connect.CodePermissionDenied}, connect.CodeOf(err))
	agent := metricsconnect.NewMetricsServiceClient(newDPoPTestClient(testAccessToken, testAgentPrivKey), testServer.URL)
	body.AgentId = uuid.NewString()
	_, err = agent.SubmitTrafficEvidence(ctx, connect.NewRequest(body))
	require.Equal(t, connect.CodePermissionDenied, connect.CodeOf(err))
	body.AgentId = testClientUUID
	event.IngressSources[1].Bytes = 9999
	_, err = agent.SubmitTrafficEvidence(ctx, connect.NewRequest(body))
	require.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(err))
	event.IngressSources[1].Bytes = 300
	for range 2 {
		response, err := agent.SubmitTrafficEvidence(ctx, connect.NewRequest(body))
		require.NoError(t, err)
		require.True(t, response.Msg.Accepted)
	}
	rows, err := trafficevidence.Current().List(ctx, testClientUUID, trafficevidence.Incident, time.Now())
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.NotContains(t, string(rows[0]), "192.0.2.2")
	_, err = agent.GetTrafficEvidenceIncident(ctx, connect.NewRequest(&metrics.GetTrafficEvidenceIncidentRequest{
		AgentId: testClientUUID, IncidentId: event.Id,
	}))
	require.Equal(t, connect.CodePermissionDenied, connect.CodeOf(err))
}
