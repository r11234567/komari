package connectapi

import (
	"context"
	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/database/trafficledger"
	"github.com/komari-monitor/komari/pkg/trafficreset"
	legacyv1 "github.com/komari-monitor/komari/protocol/v1"
	browserv1 "github.com/r11234567/komari-proto/gen/go/komari/browser/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
	"time"
)

func browserTrafficAccounting(client models.Client, report *legacyv1.Report, values map[string]trafficledger.Usage, now time.Time) *browserv1.TrafficAccounting {
	schedule := trafficreset.FromFields(client.TrafficResetDay, client.TrafficResetTime, client.TrafficResetTimezone)
	result := &browserv1.TrafficAccounting{
		ResetTime:     trafficreset.FormatClock(schedule.Hour, schedule.Minute, schedule.Second),
		ResetTimezone: schedule.Timezone,
		CountingType:  trafficledger.NormalizedCountingType(client.TrafficLimitType),
	}
	if client.TrafficResetDay != nil {
		day := int32(*client.TrafficResetDay)
		result.ResetDay = &day
	}
	if schedule.Active() {
		result.CycleStart = timestamppb.New(schedule.Last(now))
		result.NextReset = timestamppb.New(schedule.Next(now))
	}
	usage, available := values[client.UUID]
	result.CycleAvailable = available
	if !available && report != nil {
		usage = trafficledger.Usage{Up: report.Network.TotalUp, Down: report.Network.TotalDown}
	}
	result.UsedUpBytes = uint64(max(usage.Up, 0))
	result.UsedDownBytes = uint64(max(usage.Down, 0))
	result.BillableBytes = uint64(max(trafficledger.BillableUsage(result.CountingType, usage.Up, usage.Down), 0))
	return result
}

func trafficDisplayReport(ctx context.Context, id string, report *legacyv1.Report) *legacyv1.Report {
	values, err := trafficledger.CurrentCalibratedCycleUsages(ctx, dbcore.GetDBInstance(), time.Now().UTC())
	if err != nil {
		return report
	}
	return reportWithTrafficUsage(report, values, id)
}

func reportWithTrafficUsage(report *legacyv1.Report, values map[string]trafficledger.Usage, id string) *legacyv1.Report {
	usage, ok := values[id]
	if report == nil || !ok {
		return report
	}
	// The report cache and ingestion counters remain raw. Only browser DTOs
	// receive the cycle total; rates are unchanged.
	result := *report
	result.Network.TotalUp = usage.Up
	result.Network.TotalDown = usage.Down
	return &result
}
