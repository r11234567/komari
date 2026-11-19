package connectapi

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/komari-monitor/komari/database/metricstore"
	"github.com/komari-monitor/komari/pkg/metric"
	metricsv1 "github.com/r11234567/komari-proto/gen/go/komari/metrics/v1"
	metricsv1connect "github.com/r11234567/komari-proto/gen/go/komari/metrics/v1/metricsv1connect"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Occupy the real SQL read pool to stop the HTTP handler after it has acquired
// its Store, then reload while it is stopped. No production timing hook or fake
// Store is needed, and an unleased handler fails when Reload closes its Store.
func TestDualEnd_MetricReadsSurviveReload(t *testing.T) {
	for _, method := range []string{"QueryMetrics", "ListMetricDefinitions", "GetPingStats"} {
		t.Run(method, func(t *testing.T) {
			resetMetricQueryCache(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			old, release, err := metricstore.AcquireStore(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			begin := time.Now().UTC().Truncate(time.Minute).Add(-10 * time.Minute)
			if err := old.Write(ctx, metric.Point{MetricName: metricstore.MetricCPU, EntityID: testClientUUID, Timestamp: begin, Value: 42}); err != nil {
				t.Fatal(err)
			}
			release()
			stats, ok := old.PoolStats()["read"]
			if !ok || stats.MaxOpenConnections < 1 {
				t.Fatal("fixture requires a bounded independent SQLite read pool")
			}
			var held []*sql.Rows
			closeRows := func() {
				for _, rows := range held {
					_ = rows.Close()
				}
				held = nil
			}
			defer closeRows()
			for range stats.MaxOpenConnections {
				rows, err := old.QueryContext(ctx, "SELECT 1 UNION ALL SELECT 2")
				if err != nil {
					t.Fatal(err)
				}
				held = append(held, rows)
				if !rows.Next() {
					t.Fatal("could not retain SQL read connection")
				}
			}
			client := metricsv1connect.NewMetricsServiceClient(testServer.Client(), testServer.URL)
			readDone := make(chan error, 1)
			go func() {
				switch method {
				case "QueryMetrics":
					_, err := client.QueryMetrics(ctx, connect.NewRequest(&metricsv1.QueryMetricsRequest{AgentIds: []string{testClientUUID}, Metrics: []string{metricstore.MetricCPU}, StartTime: timestamppb.New(begin), EndTime: timestamppb.New(begin.Add(time.Minute)), Downsample: boolPointer(false)}))
					readDone <- err
				case "ListMetricDefinitions":
					_, err := client.ListMetricDefinitions(ctx, connect.NewRequest(&metricsv1.ListMetricDefinitionsRequest{}))
					readDone <- err
				case "GetPingStats":
					_, err := client.GetPingStats(ctx, connect.NewRequest(&metricsv1.GetPingStatsRequest{AgentIds: []string{testClientUUID}, StartTime: timestamppb.New(begin), EndTime: timestamppb.New(begin.Add(time.Minute))}))
					readDone <- err
				}
			}()
			for old.PoolStats()["read"].WaitCount <= stats.WaitCount {
				select {
				case err := <-readDone:
					t.Fatalf("HTTP read did not reach the held SQL pool: %v", err)
				case <-ctx.Done():
					t.Fatal("HTTP read did not reach SQL before deadline")
				case <-time.After(time.Millisecond):
				}
			}
			reloadDone := make(chan error, 1)
			go func() { reloadDone <- metricstore.Reload(ctx) }()
			select {
			case err := <-reloadDone:
				t.Fatalf("reload completed before the HTTP read: %v", err)
			case <-time.After(50 * time.Millisecond):
			}
			// Close marks Store closed before waiting for SQL rows. This catches
			// the old lifecycle bug even if database/sql.Close is itself blocked.
			if err := old.Ping(ctx); err != nil {
				t.Fatalf("reload retired a Store still used by HTTP: %v", err)
			}
			ingestDone := make(chan error, 1)
			go func() {
				agent := metricsv1connect.NewMetricsServiceClient(newDPoPTestClient(testAccessToken, testAgentPrivKey), testServer.URL)
				points := completeMetricPoints()
				res, err := agent.SubmitMetrics(ctx, connect.NewRequest(&metricsv1.SubmitMetricsRequest{AgentId: testClientUUID, Sequence: 77, Points: points}))
				if err == nil && res.Msg.AcceptedPoints != uint32(len(points)) {
					err = sql.ErrNoRows
				}
				ingestDone <- err
			}()
			closeRows()
			for name, done := range map[string]<-chan error{"read": readDone, "reload": reloadDone, "DPoP ingestion": ingestDone} {
				select {
				case err := <-done:
					if err != nil {
						t.Fatalf("%s failed: %v", name, err)
					}
				case <-ctx.Done():
					t.Fatalf("%s did not finish after SQL connections were released", name)
				}
			}
			resetMetricQueryCache(t)
			res, err := client.QueryMetrics(ctx, connect.NewRequest(&metricsv1.QueryMetricsRequest{AgentIds: []string{testClientUUID}, Metrics: []string{metricstore.MetricCPU}, StartTime: timestamppb.New(begin), EndTime: timestamppb.New(begin.Add(time.Minute)), Downsample: boolPointer(false)}))
			if err != nil || len(res.Msg.GetSeries()) != 1 || len(res.Msg.Series[0].QueryPoints) != 1 || res.Msg.Series[0].QueryPoints[0].GetValue() != 42 {
				t.Fatalf("post-reload HTTP history did not preserve the exact sample: response=%v error=%v", res, err)
			}
		})
	}
}

func boolPointer(value bool) *bool { return &value }
