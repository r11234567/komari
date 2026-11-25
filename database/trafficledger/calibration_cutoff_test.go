package trafficledger

import (
	"testing"
	"time"

	"github.com/komari-monitor/komari/database/metricstore"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/pkg/metric"
	"github.com/stretchr/testify/require"
)

func TestCutoffCalibrationSettlesPendingEventsOnBothSides(t *testing.T) {
	ctx := t.Context()
	db := openCalibrationOutbox(t, "fixed-cutoff")
	cutoff := time.Date(2026, 11, 15, 15, 30, 0, 0, time.UTC)
	now := cutoff.Add(5 * time.Minute)
	client := models.Client{UUID: "cutoff-node", Token: "cutoff-token", TrafficResetDay: intPointer(1), TrafficLimitType: "max"}
	require.NoError(t, db.Create(&client).Error)
	store, release, err := metricstore.AcquireStore(ctx)
	require.NoError(t, err)
	defer release()
	telemetry := store.Telemetry()
	accept := func(sequence int64, offset time.Duration, up, down int64) metric.TelemetryReceipt {
		at := cutoff.Add(offset)
		batch := metric.TelemetryBatch{Source: client.UUID, Identity: at.String(), TrafficSample: &metric.TrafficSample{
			Stream: "agent-spool", Sequence: sequence, Observed: at.UnixNano(), BootID: "a",
			Interfaces: map[string]metric.TrafficCounters{"eth0": {Sent: up, Received: down}},
		}}
		result, err := telemetry.Accept(ctx, batch, now)
		require.NoError(t, err)
		return result
	}
	accept(1, -time.Minute, 0, 0)
	accept(2, -30*time.Second, 100, 80)
	_, err = telemetry.DeliverTraffic(ctx, 8, ConsumeTelemetry)
	require.NoError(t, err)
	accept(3, 30*time.Second, 100, 160)
	require.True(t, accept(3, 30*time.Second, 100, 160).Duplicate)
	_, cycle, err := CurrentTrafficCycleFor(client, now)
	require.NoError(t, err)
	total := int64(500)
	before, err := SaveTrafficCutoff(ctx, db, client, cycle, cutoff, Usage{}, &total, "max", "admin", "provider", now)
	require.NoError(t, err)
	require.EqualValues(t, 500, BillableUsage("max", before.Effective.Up, before.Effective.Down))
	pending, err := telemetry.TrafficPendingFor(ctx, client.UUID)
	require.NoError(t, err)
	require.True(t, pending)
	n, err := telemetry.DeliverTraffic(ctx, 8, ConsumeTelemetry)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	after, err := LoadCalibrationSnapshot(ctx, db, client, now)
	require.NoError(t, err)
	require.Equal(t, Usage{Up: 100, Down: 160}, after.Raw)
	require.EqualValues(t, 520, BillableUsage("max", after.Effective.Up, after.Effective.Down))
	require.Equal(t, cutoff.Format(time.RFC3339), after.History[0].Cutoff)
}

func TestTrafficPolicyChangeClosesOldCycleAtExistingBoundary(t *testing.T) {
	ctx := t.Context()
	db := openCalibrationOutbox(t, "policy-boundary")
	client := models.Client{UUID: "policy-node", Token: "policy-token", TrafficResetDay: intPointer(15), TrafficResetTime: "15:30:17", TrafficResetTimezone: "UTC", TrafficLimitType: "sum"}
	require.NoError(t, db.Create(&client).Error)
	boundary := time.Date(2026, 11, 15, 15, 30, 17, 0, time.UTC)
	now := boundary.Add(-time.Minute)
	require.NoError(t, SaveTrafficPolicy(ctx, db, client.UUID, 20, "11:30:00", "UTC", now))
	require.NoError(t, db.First(&client, "uuid = ?", client.UUID).Error)
	before, err := TrafficScheduleAt(ctx, db, client, now)
	require.NoError(t, err)
	require.Equal(t, boundary, before.Next(now))
	after, err := TrafficScheduleAt(ctx, db, client, boundary)
	require.NoError(t, err)
	require.Equal(t, boundary, after.Last(boundary))
	require.Equal(t, time.Date(2026, 11, 20, 11, 30, 0, 0, time.UTC), after.Next(boundary))
	require.NoError(t, ApplyTelemetry(ctx, db, metric.TelemetryTrafficEvent{
		StoreID: "test", IngestID: 1, Source: client.UUID, Observed: boundary.Add(10 * time.Second), Received: boundary.Add(time.Minute),
		Traffic: metric.TelemetryTraffic{HasDelta: true, DeltaUp: 101, Start: boundary.Add(-10 * time.Second).UnixNano()},
	}))
	var cycles []models.TelemetryTrafficCycle
	require.NoError(t, db.Where("client = ?", client.UUID).Order("start").Find(&cycles).Error)
	require.Len(t, cycles, 2)
	require.EqualValues(t, 50, cycles[0].UpBytes)
	require.EqualValues(t, 51, cycles[1].UpBytes)
	require.Equal(t, boundary.UnixNano(), cycles[1].Start)
}
