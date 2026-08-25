package jsonrpc

// Dashboard aggregates for the typed Connect transport.
//
// komari.admin.v1.DashboardService is the production reader for the
// administration dashboard; the legacy admin:getDashboard* methods stay
// registered only so unconverted themes keep working over /api/rpc2. Both
// transports go through the aggregation below so they can never disagree.
//
// The aliases expose the existing response shapes without renaming them in
// place. When the aggregation itself moves out of this package into
// web/dashboard, these aliases and wrappers move with it and the Connect
// adapter only changes its import.

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/komari-monitor/komari/database/clients"
	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/models"
	agent_runtime "github.com/komari-monitor/komari/web/agent"
)

type (
	DashboardSummaryResult    = dashboardResponse
	DashboardChartsResult     = dashboardChartsResponse
	DashboardAlertItemsResult = dashboardAlertItemsResponse

	DashboardServers          = dashboardServerSummary
	DashboardOfflineNode      = dashboardOfflineNode
	DashboardResources        = dashboardResourceSummary
	DashboardResourceRankItem = dashboardResourceRankItem

	DashboardDatabase      = databaseStatusResponse
	DashboardDatabaseStore = databaseStorageStatus
	DashboardDatabaseFiles = databaseFileSizes
	DashboardStorage       = dashboardStorageSummary
	DashboardReturnRoute   = dashboardReturnRouteSummary

	DashboardAlerts       = dashboardAlertSummaries
	DashboardAlertSummary = dashboardAlertSummary
	DashboardAlertItem    = dashboardAlertAffectedItem
	DashboardAlertLatest  = dashboardAlertLatest

	DashboardTraffic         = dashboardTrafficSummary
	DashboardTrafficBucket   = dashboardTrafficBucket
	DashboardTrafficDay      = dashboardTrafficDay
	DashboardTrafficRankItem = dashboardTrafficRankItem

	DashboardLatency               = dashboardLatencySummary
	DashboardLatencyPoint          = dashboardLatencyPoint
	DashboardLatencyRankItem       = dashboardLatencyRankItem
	DashboardLatencyJitterRankItem = dashboardLatencyJitterRankItem

	DashboardPacketLoss         = dashboardPacketLossSummary
	DashboardPacketLossRankItem = dashboardPacketLossRankItem

	DatabaseMaintenanceReport  = databaseMaintenanceResponse
	DatabaseMaintenanceOutcome = databaseMaintenanceResult
)

// ErrDatabaseMaintenanceBusy reports a reclaim that is already running.
var ErrDatabaseMaintenanceBusy = errors.New("database maintenance is already in progress")

// DatabaseStatus reports driver, location and size for both stores. Type and
// Size stay populated from the main store to preserve the original contract.
func DatabaseStatus(ctx context.Context) DashboardDatabase {
	main := mainDatabaseStatus()
	monitoring := monitoringDatabaseStatus(ctx)
	legacySize := int64(0)
	if main.Size != nil {
		legacySize = *main.Size
	}
	return DashboardDatabase{
		Type:       main.Driver,
		Size:       legacySize,
		Main:       main,
		Monitoring: monitoring,
		LocalTotal: localDatabaseTotal(main, monitoring),
	}
}

// VacuumDatabases reclaims storage in both stores. Auditing is left to the
// caller so each transport records its own actor.
func VacuumDatabases(ctx context.Context) (DatabaseMaintenanceReport, error) {
	if !databaseMaintenanceMu.TryLock() {
		return DatabaseMaintenanceReport{}, ErrDatabaseMaintenanceBusy
	}
	defer databaseMaintenanceMu.Unlock()
	return newDatabaseMaintenanceResponse(maintainMainDatabase(ctx), maintainMonitoringDatabase(ctx)), nil
}

// AuditLogPage returns one page of audit entries together with the total count.
func AuditLogPage(limit, page int, messageType string) ([]models.Log, int64, error) {
	return queryAdminLogs(dbcore.GetDBInstance(), limit, page, messageType)
}

// DashboardSection selects one summary section without exposing the internal bitmask.
type DashboardSection int

const (
	DashboardSectionServersSelector DashboardSection = iota + 1
	DashboardSectionResourcesSelector
	DashboardSectionStorageSelector
	DashboardSectionReturnRouteSelector
	DashboardSectionAlertsSelector
)

// DashboardChart selects one chart series without exposing the internal bitmask.
type DashboardChart int

const (
	DashboardChartTrafficSelector DashboardChart = iota + 1
	DashboardChartLatencySelector
	DashboardChartLatencyJitterSelector
	DashboardChartPacketLossSelector
)

// ErrDashboardAlertKind reports an alert kind outside the supported set.
var ErrDashboardAlertKind = errors.New("unsupported dashboard alert kind")

// DashboardRankingLimit normalizes a requested ranking size exactly like the
// legacy limit query parameter, so both transports rank identically.
func DashboardRankingLimit(value int) int {
	if dashboardRankingLimitAllowed(value) {
		return value
	}
	return 5
}

func dashboardSummaryMask(sections []DashboardSection) dashboardSummarySections {
	mask := dashboardSummarySections(0)
	for _, section := range sections {
		switch section {
		case DashboardSectionServersSelector:
			mask |= dashboardSectionServers
		case DashboardSectionResourcesSelector:
			mask |= dashboardSectionResources
		case DashboardSectionStorageSelector:
			mask |= dashboardSectionStorage
		case DashboardSectionReturnRouteSelector:
			mask |= dashboardSectionReturnRoute
		case DashboardSectionAlertsSelector:
			mask |= dashboardSectionAlerts
		}
	}
	if mask == 0 {
		return dashboardSectionAll
	}
	return mask
}

func dashboardChartMask(charts []DashboardChart) dashboardChartSections {
	mask := dashboardChartSections(0)
	for _, chart := range charts {
		switch chart {
		case DashboardChartTrafficSelector:
			mask |= dashboardChartTraffic
		case DashboardChartLatencySelector:
			mask |= dashboardChartLatency
		case DashboardChartLatencyJitterSelector:
			mask |= dashboardChartLatencyJitter
		case DashboardChartPacketLossSelector:
			mask |= dashboardChartPacketLoss
		}
	}
	if mask == 0 {
		return dashboardChartAll
	}
	return mask
}

// DashboardSummaryAggregate returns the cached summary for the requested
// sections. An empty selection returns every section.
func DashboardSummaryAggregate(ctx context.Context, sections []DashboardSection, rankingLimit int) (DashboardSummaryResult, error) {
	settings := loadDashboardSettings()
	value, err := buildDashboardCached(
		ctx,
		time.Now().UTC(),
		dashboardSummaryMask(sections),
		DashboardRankingLimit(rankingLimit),
		time.Duration(settings.RefreshSeconds)*time.Second,
	)
	if err != nil {
		return DashboardSummaryResult{}, err
	}
	return decorateDashboardSummaryNavigation(value), nil
}

// DashboardChartsAggregate returns the cached chart series for the requested
// selection. An empty selection returns every series.
func DashboardChartsAggregate(ctx context.Context, charts []DashboardChart, rankingLimit int) DashboardChartsResult {
	settings := loadDashboardSettings()
	return decorateDashboardNavigation(buildDashboardChartsCached(
		ctx,
		time.Now().UTC(),
		dashboardChartMask(charts),
		DashboardRankingLimit(rankingLimit),
		time.Duration(settings.ChartRefreshSeconds)*time.Second,
	))
}

// DashboardAlertItems lists the currently affected targets for one alert kind.
// A partially failed alert family is reported as an error rather than as an
// empty list, so the caller never renders "no alerts" over a collection failure.
func DashboardAlertItems(kind string) (DashboardAlertItemsResult, error) {
	normalized := strings.ToLower(strings.TrimSpace(kind))
	if _, ok := dashboardAlertKinds[normalized]; !ok {
		return DashboardAlertItemsResult{}, ErrDashboardAlertKind
	}
	clientList, err := clients.GetAllClientBasicInfo()
	if err != nil {
		return DashboardAlertItemsResult{}, err
	}
	now := time.Now().UTC()
	clientByID := make(map[string]models.Client, len(clientList))
	for _, client := range clientList {
		clientByID[client.UUID] = client
	}
	reports := agent_runtime.GetLatestReport()
	var summary dashboardAlertSummary
	switch normalized {
	case "offline":
		summary = buildDashboardOfflineAlerts(clientByID, now)
	case "resource":
		summary = buildDashboardResourceAlerts(clientByID, reports)
	case "latency_loss":
		summary = buildDashboardLatencyAlerts(now)
	case "traffic":
		summary = buildDashboardTrafficAlerts(clientList, reports, now)
	case "return_route":
		summary = buildDashboardReturnRouteAlerts(now)
	case "billing":
		summary = buildDashboardBillingAlerts(clientList, now)
	}
	if summary.Error != "" {
		return DashboardAlertItemsResult{}, errors.New(summary.Error)
	}
	items := summary.Items
	if items == nil {
		items = []dashboardAlertAffectedItem{}
	}
	return DashboardAlertItemsResult{Kind: normalized, Items: items, GeneratedAt: now}, nil
}
