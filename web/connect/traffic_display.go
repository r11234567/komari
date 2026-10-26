package connectapi

import (
	"context"
	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/trafficledger"
	legacyv1 "github.com/komari-monitor/komari/protocol/v1"
	"time"
)

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
