package connectapi

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/komari-monitor/komari/database/clients"
	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/pkg/rpc"
	metricsv1 "github.com/r11234567/komari-proto/gen/go/komari/metrics/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestConnectMetricEmptyVisibilityReturnsBeforeStorePlanning(t *testing.T) {
	ctx := rpc.NewContextWithMeta(context.Background(), &rpc.ContextMeta{Principal: rpc.NewAnonymousPrincipal()})
	service := &metricsService{}
	end := time.Now().UTC()
	start := end.Add(-time.Hour)
	// Hidden agents and explicit nonmatching IDs both resolve to no visible nodes.
	prior, err := clients.GetClientByUUID(testClientUUID)
	if err != nil {
		t.Fatal(err)
	}
	if err := dbcore.GetDBInstance().Model(&models.Client{}).Where("uuid = ?", testClientUUID).Update("hidden", true).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := dbcore.GetDBInstance().Model(&models.Client{}).Where("uuid = ?", testClientUUID).Update("hidden", prior.Hidden).Error; err != nil {
			t.Error(err)
		}
	})
	for _, ids := range [][]string{{testClientUUID}, {"projection-unknown-agent"}} {
		metricResponse, err := service.queryMetrics(ctx, connect.NewRequest(&metricsv1.QueryMetricsRequest{AgentIds: ids, Metrics: []string{"projection-unknown-metric"}, StartTime: timestamppb.New(start), EndTime: timestamppb.New(end)}))
		if err != nil || len(metricResponse.Msg.Series) != 0 {
			t.Fatalf("empty visibility reached metric lookup: %+v %v", metricResponse, err)
		}
		pingResponse, err := service.getPingStats(ctx, connect.NewRequest(&metricsv1.GetPingStatsRequest{AgentIds: ids, StartTime: timestamppb.New(start), EndTime: timestamppb.New(end)}))
		if err != nil {
			t.Fatal(err)
		}
		if len(pingResponse.Msg.Stats) != 0 || !pingResponse.Msg.StartTime.AsTime().Equal(start) || !pingResponse.Msg.EndTime.AsTime().Equal(end) {
			t.Fatalf("empty ping changed requested window: %+v", pingResponse.Msg)
		}
	}
}
