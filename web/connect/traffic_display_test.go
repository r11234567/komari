package connectapi

import (
	"connectrpc.com/connect"
	"context"
	"github.com/komari-monitor/komari/database/clients"
	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/metricstore"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/database/trafficledger"
	legacyv1 "github.com/komari-monitor/komari/protocol/v1"
	agentRuntime "github.com/komari-monitor/komari/web/agent"
	browserv1 "github.com/r11234567/komari-proto/gen/go/komari/browser/v1"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestTrafficDisplayLeavesAgentCountersAndSpeedUnchanged(t *testing.T) {
	raw := &legacyv1.Report{Network: legacyv1.NetworkReport{TotalUp: 123, TotalDown: 456, Up: 7, Down: 8}}
	displayed := reportWithTrafficUsage(raw, map[string]trafficledger.Usage{"node": {Up: 500, Down: 600}}, "node")
	require.EqualValues(t, 123, raw.Network.TotalUp)
	require.EqualValues(t, 456, raw.Network.TotalDown)
	require.EqualValues(t, 500, displayed.Network.TotalUp)
	require.EqualValues(t, 600, displayed.Network.TotalDown)
	require.Equal(t, raw.Network.Up, displayed.Network.Up)
	require.Equal(t, raw.Network.Down, displayed.Network.Down)
	require.NotEqual(t, browserStatusVersion(&browserv1.AgentSummary{}, raw), browserStatusVersion(&browserv1.AgentSummary{}, displayed))
}

func TestTrafficAccountingScheduleAndUnavailableHistory(t *testing.T) {
	day := 31
	client := models.Client{UUID: "node", TrafficResetDay: &day, TrafficResetTime: "12:45:00", TrafficResetTimezone: "UTC-05:30", TrafficLimitType: "sum"}
	now := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	report := &legacyv1.Report{Network: legacyv1.NetworkReport{TotalUp: 21_230_000_000, TotalDown: 5_000_000_000}}
	missing := browserTrafficAccounting(client, report, nil, now)
	require.False(t, missing.CycleAvailable, "missing cycle data must not be presented as calibrated")
	require.EqualValues(t, 26_230_000_000, missing.BillableBytes)
	require.Equal(t, time.Date(2026, 11, 30, 18, 15, 0, 0, time.UTC), missing.NextReset.AsTime())
	require.Equal(t, "UTC-05:30", missing.ResetTimezone)
	values := map[string]trafficledger.Usage{"node": {Up: 50_720_000_000, Down: 5_000_000_000}}
	accounting := browserTrafficAccounting(client, report, values, now)
	require.True(t, accounting.CycleAvailable)
	require.EqualValues(t, 55_720_000_000, accounting.BillableBytes)
	require.EqualValues(t, 21_230_000_000, report.Network.TotalUp)
	before := &browserv1.AgentSummary{TrafficAccounting: accounting}
	after := &browserv1.AgentSummary{TrafficAccounting: browserTrafficAccounting(client, report, values, accounting.NextReset.AsTime())}
	require.NotEqual(t, browserStatusVersion(before, report), browserStatusVersion(after, report), "reset must update the stream even without an Agent report")
}

// Exercise the real persistent calibration followed by the actual browser
// handlers, not a pre-filled display DTO. No external network or delivery.
func TestTrafficCalibrationReachesListDetailAndRealCounterGrowth(t *testing.T) {
	ctx := context.Background()
	id, _, err := clients.CreateClientWithName("synthetic traffic accounting")
	require.NoError(t, err)
	t.Cleanup(func() { agentRuntime.DeleteLatestReport(id); trafficledger.InvalidateCalibratedCycleCache() })
	now := time.Now().UTC().Truncate(time.Second)
	start := now.Add(-time.Minute)
	day := start.Day()
	db := dbcore.GetDBInstance()
	require.NoError(t, db.Model(&models.Client{}).Where("uuid = ?", id).Updates(map[string]any{
		"traffic_reset_day": day, "traffic_reset_time": start.Format("15:04:05"), "traffic_reset_timezone": "UTC", "traffic_limit_type": "sum",
	}).Error)
	var client models.Client
	require.NoError(t, db.First(&client, "uuid = ?", id).Error)
	raw := legacyv1.Report{UUID: id, UpdatedAt: now.Add(-20 * time.Second), Network: legacyv1.NetworkReport{TotalUp: 21_230_000_000, TotalDown: 9_000_000_000, Up: 2_000_000_000, Down: 2_000_000_000}}
	_, err = metricstore.WriteReport(ctx, raw)
	require.NoError(t, err)
	require.NoError(t, metricstore.FlushReportBatch(ctx))
	agentRuntime.RecordReport(raw)
	target := trafficledger.Usage{Up: 50_720_000_000, Down: 5_000_000_000}
	_, err = trafficledger.CalibrateCurrentCycle(ctx, db, client, target, "synthetic administrator", now.Add(-10*time.Second))
	require.NoError(t, err)
	service := &browserService{}
	verify := func(want trafficledger.Usage) {
		t.Helper()
		detail, err := service.GetAgent(ctx, connect.NewRequest(&browserv1.GetAgentRequest{AgentId: id}))
		require.NoError(t, err)
		require.EqualValues(t, want.Up, detail.Msg.Agent.TrafficAccounting.UsedUpBytes)
		require.EqualValues(t, want.Down, detail.Msg.LatestReport.NetworkInterfaces[0].BytesReceived)
		require.True(t, detail.Msg.Agent.TrafficAccounting.CycleAvailable)
		list, err := service.ListAgents(ctx, connect.NewRequest(&browserv1.ListAgentsRequest{AgentIds: []string{id}}))
		require.NoError(t, err)
		require.Len(t, list.Msg.Agents, 1)
		require.Equal(t, detail.Msg.Agent.TrafficAccounting, list.Msg.Agents[0].TrafficAccounting)
		used, err := trafficledger.CurrentCalibratedCycleUsages(ctx, db, time.Now().UTC())
		require.NoError(t, err)
		require.EqualValues(t, trafficledger.BillableUsage("sum", used[id].Up, used[id].Down), detail.Msg.Agent.TrafficAccounting.BillableBytes, "browser and backend alert counting must share the same totals")
	}
	verify(target)
	raw.UpdatedAt = now.Add(-time.Second)
	raw.Network.TotalUp += 5_000_000_000
	raw.Network.TotalDown += 5_000_000_000
	_, err = metricstore.WriteReport(ctx, raw)
	require.NoError(t, err)
	require.NoError(t, metricstore.FlushReportBatch(ctx))
	agentRuntime.RecordReport(raw)
	trafficledger.InvalidateCalibratedCycleCache()
	verify(trafficledger.Usage{Up: target.Up + 5_000_000_000, Down: target.Down + 5_000_000_000})
	require.Equal(t, raw.Network, agentRuntime.GetLatestReport()[id].Network, "calibration must not rewrite the Agent source or its ingestion baseline")
}
