package jsonrpc

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/komari-monitor/komari/database/clients"
	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/metricstore"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/database/tasks"
	"github.com/komari-monitor/komari/database/trafficledger"
	"github.com/komari-monitor/komari/pkg/rpc"
	agent_runtime "github.com/komari-monitor/komari/web/agent"
	dashboardapp "github.com/komari-monitor/komari/web/dashboard"
	"gorm.io/gorm"
)

type dashboardOfflineNode struct {
	UUID     string     `json:"uuid"`
	Name     string     `json:"name"`
	Region   string     `json:"region"`
	LastSeen *time.Time `json:"last_seen"`
}

type dashboardServerSummary struct {
	Total        int                    `json:"total"`
	Online       int                    `json:"online"`
	Offline      int                    `json:"offline"`
	OfflineNodes []dashboardOfflineNode `json:"offline_nodes"`
}

type dashboardTrafficDay struct {
	Day       string `json:"day"`
	Up        int64  `json:"up"`
	Down      int64  `json:"down"`
	Billable  int64  `json:"billable"`
	Available *bool  `json:"available,omitempty"`
}

type dashboardTrafficBucket struct {
	Hour string `json:"hour"`
	Up   int64  `json:"up"`
	Down int64  `json:"down"`
}

type dashboardTrafficRankItem struct {
	UUID      string `json:"uuid"`
	Name      string `json:"name"`
	Up        int64  `json:"up"`
	Down      int64  `json:"down"`
	Billable  int64  `json:"billable"`
	DetailURL string `json:"detail_url,omitempty"`
}

type dashboardTrafficSummary struct {
	TodayUp           int64                      `json:"today_up"`
	TodayDown         int64                      `json:"today_down"`
	TodayBillable     int64                      `json:"today_billable"`
	Hourly            []dashboardTrafficBucket   `json:"hourly"`
	Daily             []dashboardTrafficDay      `json:"daily"`
	Ranking           []dashboardTrafficRankItem `json:"ranking"`
	HistoryReady      bool                       `json:"history_ready"`
	Error             string                     `json:"error,omitempty"`
	TodayError        string                     `json:"today_error,omitempty"`
	TrendError        string                     `json:"trend_error,omitempty"`
	RankingError      string                     `json:"ranking_error,omitempty"`
	AccountingThrough *time.Time                 `json:"accounting_through,omitempty"`
	CatchingUp        bool                       `json:"catching_up"`
	TrendCoverage     string                     `json:"trend_coverage,omitempty"`
}

type dashboardStorageSummary struct {
	DatabaseFiles   int64      `json:"database_files"`
	WAL             int64      `json:"wal"`
	SHM             int64      `json:"shm"`
	RetentionDays   int        `json:"retention_days"`
	LastCompactedAt *time.Time `json:"last_compacted_at"`
}

type dashboardReturnRouteSummary struct {
	tasks.ReturnRouteSummary
	LatestEvent *tasks.ReturnRouteEventItem `json:"latest_event,omitempty"`
	Error       string                      `json:"error,omitempty"`
}

type dashboardResourceRankItem struct {
	UUID      string  `json:"uuid"`
	Name      string  `json:"name"`
	CPU       float64 `json:"cpu"`
	Memory    float64 `json:"memory"`
	Disk      float64 `json:"disk"`
	DetailURL string  `json:"detail_url,omitempty"`
}

type dashboardResourceSummary struct {
	CPU    []dashboardResourceRankItem `json:"cpu"`
	Memory []dashboardResourceRankItem `json:"memory"`
	Disk   []dashboardResourceRankItem `json:"disk"`
}

type dashboardResponse struct {
	Servers     dashboardServerSummary      `json:"servers"`
	Resources   dashboardResourceSummary    `json:"resources"`
	Database    databaseStatusResponse      `json:"database"`
	Storage     dashboardStorageSummary     `json:"storage"`
	ReturnRoute dashboardReturnRouteSummary `json:"return_route"`
	Alerts      dashboardAlertSummaries     `json:"alerts"`
	GeneratedAt time.Time                   `json:"generated_at"`
}

type dashboardSummarySections uint8

const (
	dashboardSectionServers dashboardSummarySections = 1 << iota
	dashboardSectionResources
	dashboardSectionStorage
	dashboardSectionReturnRoute
	dashboardSectionAlerts
	dashboardSectionAll = dashboardSectionServers | dashboardSectionResources |
		dashboardSectionStorage | dashboardSectionReturnRoute | dashboardSectionAlerts
)

type dashboardChartSections uint8

const (
	dashboardChartTraffic dashboardChartSections = 1 << iota
	dashboardChartLatency
	dashboardChartLatencyJitter
	dashboardChartPacketLoss
	dashboardChartAll = dashboardChartTraffic | dashboardChartLatency | dashboardChartLatencyJitter | dashboardChartPacketLoss
)

type dashboardChartsResponse struct {
	Traffic     dashboardTrafficSummary    `json:"traffic"`
	Latency     dashboardLatencySummary    `json:"latency"`
	PacketLoss  dashboardPacketLossSummary `json:"packet_loss"`
	GeneratedAt time.Time                  `json:"generated_at"`
}

func init() {
	RegisterWithGroupAndMeta("getDashboard", rpc.RoleAdmin, adminGetDashboard, &rpc.MethodMeta{
		Name:    "admin:getDashboard",
		Summary: "Get the cached administration dashboard summary",
		Returns: "DashboardSummary",
	})
	RegisterWithGroupAndMeta("getDashboardCharts", rpc.RoleAdmin, adminGetDashboardCharts, &rpc.MethodMeta{
		Name:    "admin:getDashboardCharts",
		Summary: "Get cached administration dashboard chart data",
		Returns: "DashboardCharts",
	})
	RegisterWithGroupAndMeta("getDashboardAlertItems", rpc.RoleAdmin, adminGetDashboardAlertItems, &rpc.MethodMeta{
		Name:    "admin:getDashboardAlertItems",
		Summary: "List currently affected dashboard alert targets",
		Returns: "DashboardAlertItems",
	})
}

func adminGetDashboard(ctx context.Context, req *rpc.JsonRpcRequest) (any, *rpc.JsonRpcError) {
	now := time.Now().UTC()
	sections, rankingLimit := parseDashboardSummaryRequest(req)
	settings := loadDashboardSettings()
	value, err := buildDashboardCached(ctx, now, sections, rankingLimit, time.Duration(settings.RefreshSeconds)*time.Second)
	if err != nil {
		return nil, rpc.MakeError(rpc.InternalError, err.Error(), nil)
	}
	return decorateDashboardSummaryNavigation(value), nil
}

func adminGetDashboardCharts(ctx context.Context, req *rpc.JsonRpcRequest) (any, *rpc.JsonRpcError) {
	now := time.Now().UTC()
	sections, rankingLimit := parseDashboardChartRequest(req)
	settings := loadDashboardSettings()
	return decorateDashboardNavigation(buildDashboardChartsCached(ctx, now, sections, rankingLimit, time.Duration(settings.ChartRefreshSeconds)*time.Second)), nil
}

func buildDashboard(ctx context.Context, now time.Time, sections dashboardSummarySections, rankingLimit int) (dashboardResponse, error) {
	result := dashboardResponse{GeneratedAt: now}
	needsClients := sections&(dashboardSectionServers|dashboardSectionResources|dashboardSectionAlerts) != 0
	var clientList []models.Client
	var err error
	if needsClients {
		clientList, err = clients.GetAllClientBasicInfo()
		if err != nil {
			return dashboardResponse{}, fmt.Errorf("list dashboard clients: %w", err)
		}
	}
	if sections&dashboardSectionServers != 0 {
		result.Servers = buildDashboardServers(clientList)
	}
	if sections&dashboardSectionResources != 0 {
		result.Resources = buildDashboardResources(clientList, rankingLimit)
	}
	if sections&dashboardSectionStorage != 0 {
		main := mainDatabaseStatus()
		monitoring := monitoringDatabaseStatus(ctx)
		legacySize := int64(0)
		if main.Size != nil {
			legacySize = *main.Size
		}
		result.Database = databaseStatusResponse{
			Type:       main.Driver,
			Size:       legacySize,
			Main:       main,
			Monitoring: monitoring,
			LocalTotal: localDatabaseTotal(main, monitoring),
		}
		result.Storage = buildDashboardStorage(ctx, main, monitoring)
	}
	if sections&dashboardSectionReturnRoute != 0 {
		result.ReturnRoute = buildDashboardReturnRoute()
	}
	if sections&dashboardSectionAlerts != 0 {
		result.Alerts = buildDashboardAlerts(clientList, now)
	}
	return result, nil
}

func buildDashboardCharts(ctx context.Context, now time.Time, sections dashboardChartSections, rankingLimit int) dashboardChartsResponse {
	result := dashboardChartsResponse{GeneratedAt: now}
	if sections == 0 {
		return result
	}
	clientList, err := clients.GetAllClientBasicInfo()
	if err != nil {
		message := fmt.Sprintf("list dashboard clients: %v", err)
		if sections&dashboardChartTraffic != 0 {
			result.Traffic.Error = message
		}
		if sections&dashboardChartLatency != 0 {
			result.Latency.Error = message
		}
		if sections&dashboardChartLatencyJitter != 0 {
			result.Latency.JitterError = message
		}
		if sections&dashboardChartPacketLoss != 0 {
			result.PacketLoss.Error = message
		}
		return result
	}
	if sections&dashboardChartTraffic != 0 {
		if result.Traffic, err = loadDashboardTraffic(ctx, clientList, now, rankingLimit); err != nil {
			result.Traffic = dashboardTrafficSummary{Error: err.Error()}
		}
	}
	if sections&dashboardChartLatency != 0 {
		if result.Latency, err = loadDashboardLatency(ctx, clientList, now, rankingLimit); err != nil {
			result.Latency.Error = err.Error()
		}
	}
	if sections&dashboardChartLatencyJitter != 0 {
		if result.Latency.JitterRanking, err = loadDashboardLatencyJitter(ctx, clientList, now, rankingLimit); err != nil {
			result.Latency.JitterError = err.Error()
		}
	}
	if sections&dashboardChartPacketLoss != 0 {
		if result.PacketLoss, err = loadDashboardPacketLoss(ctx, clientList, now, rankingLimit); err != nil {
			result.PacketLoss.Error = err.Error()
		}
	}
	return result
}

func parseDashboardSummaryRequest(req *rpc.JsonRpcRequest) (dashboardSummarySections, int) {
	rawSections, _ := rpc.GetParamAs[string](req, "sections")
	sections := dashboardSummarySections(0)
	if strings.TrimSpace(rawSections) == "" {
		sections = dashboardSectionAll
	} else {
		for _, raw := range strings.Split(rawSections, ",") {
			switch strings.TrimSpace(raw) {
			case "servers":
				sections |= dashboardSectionServers
			case "resources":
				sections |= dashboardSectionResources
			case "storage":
				sections |= dashboardSectionStorage
			case "return_route":
				sections |= dashboardSectionReturnRoute
			case "alerts":
				sections |= dashboardSectionAlerts
			}
		}
	}

	return sections, parseDashboardRankingLimit(req)
}

func parseDashboardChartRequest(req *rpc.JsonRpcRequest) (dashboardChartSections, int) {
	rawSections, _ := rpc.GetParamAs[string](req, "sections")
	if strings.TrimSpace(rawSections) == "" {
		return dashboardChartAll, parseDashboardRankingLimit(req)
	}
	sections := dashboardChartSections(0)
	for _, raw := range strings.Split(rawSections, ",") {
		switch strings.TrimSpace(raw) {
		case "traffic":
			sections |= dashboardChartTraffic
		case "latency":
			sections |= dashboardChartLatency
		case "latency_jitter":
			sections |= dashboardChartLatencyJitter
		case "packet_loss":
			sections |= dashboardChartPacketLoss
		}
	}
	return sections, parseDashboardRankingLimit(req)
}

func parseDashboardRankingLimit(req *rpc.JsonRpcRequest) int {
	if rawLimit, ok := rpc.GetParamAs[string](req, "limit"); ok {
		if parsed, err := strconv.Atoi(rawLimit); err == nil && dashboardRankingLimitAllowed(parsed) {
			return parsed
		}
	}
	return 5
}

func buildDashboardResources(clientList []models.Client, limit int) dashboardResourceSummary {
	if !dashboardRankingLimitAllowed(limit) {
		limit = 5
	}
	reports := agent_runtime.GetLatestReport()
	items := make([]dashboardResourceRankItem, 0, len(clientList))
	for _, client := range clientList {
		report := reports[client.UUID]
		if report == nil {
			continue
		}
		name := strings.TrimSpace(client.Name)
		if name == "" {
			name = client.UUID
		}
		items = append(items, dashboardResourceRankItem{
			UUID:   client.UUID,
			Name:   name,
			CPU:    dashboardPercent(report.CPU.Usage),
			Memory: dashboardUsagePercent(report.Ram.Used, report.Ram.Total),
			Disk:   dashboardUsagePercent(report.Disk.Used, report.Disk.Total),
		})
	}
	return dashboardResourceSummary{
		CPU:    dashboardTopResources(items, limit, func(item dashboardResourceRankItem) float64 { return item.CPU }),
		Memory: dashboardTopResources(items, limit, func(item dashboardResourceRankItem) float64 { return item.Memory }),
		Disk:   dashboardTopResources(items, limit, func(item dashboardResourceRankItem) float64 { return item.Disk }),
	}
}

func dashboardUsagePercent(used, total int64) float64 {
	if used <= 0 || total <= 0 {
		return 0
	}
	return dashboardPercent(float64(used) / float64(total) * 100)
}

func dashboardPercent(value float64) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) || value <= 0 {
		return 0
	}
	return math.Min(100, value)
}

// dashboardTopResources keeps only a bounded Top-N list while scanning reports.
// With at most 20 entries this avoids three full sorts and large temporary slices.
func dashboardTopResources(
	items []dashboardResourceRankItem,
	limit int,
	value func(dashboardResourceRankItem) float64,
) []dashboardResourceRankItem {
	top := make([]dashboardResourceRankItem, 0, limit)
	for _, item := range items {
		insertAt := len(top)
		for index, current := range top {
			if value(item) > value(current) || (value(item) == value(current) && item.Name < current.Name) {
				insertAt = index
				break
			}
		}
		if insertAt >= limit {
			continue
		}
		if len(top) < limit {
			top = append(top, dashboardResourceRankItem{})
		}
		copy(top[insertAt+1:], top[insertAt:len(top)-1])
		top[insertAt] = item
	}
	return top
}

func buildDashboardReturnRoute() dashboardReturnRouteSummary {
	result := dashboardReturnRouteSummary{}
	summary, err := tasks.GetReturnRouteSummary()
	if err != nil {
		result.Error = err.Error()
		return result
	}
	result.ReturnRouteSummary = summary

	events, err := tasks.QueryReturnRouteEvents(tasks.ReturnRouteEventQuery{Page: 1, PageSize: 1})
	if err != nil {
		result.Error = err.Error()
		return result
	}
	if len(events.Events) > 0 {
		latest := events.Events[0]
		result.LatestEvent = &latest
	}
	return result
}

func buildDashboardStorage(ctx context.Context, statuses ...databaseStorageStatus) dashboardStorageSummary {
	summary := dashboardStorageSummary{}
	for _, status := range statuses {
		if status.Location != databaseLocationLocal || status.Files == nil {
			continue
		}
		summary.DatabaseFiles += status.Files.Database
		summary.WAL += status.Files.WAL
		summary.SHM += status.Files.SHM
		if status.Runtime != nil && status.Runtime.LastCycleCompletedAt != nil {
			completed := status.Runtime.LastCycleCompletedAt.UTC()
			if summary.LastCompactedAt == nil || completed.After(*summary.LastCompactedAt) {
				summary.LastCompactedAt = &completed
			}
		}
	}
	if retention, err := metricstore.GetRetentionSummary(ctx); err == nil {
		summary.RetentionDays = retention.MaxDays
	}
	return summary
}

func buildDashboardServers(clientList []models.Client) dashboardServerSummary {
	onlineSet := make(map[string]struct{})
	for _, uuid := range agent_runtime.GetAllOnlineUUIDs() {
		onlineSet[uuid] = struct{}{}
	}
	latest := agent_runtime.GetLatestReport()
	summary := dashboardServerSummary{
		Total:        len(clientList),
		OfflineNodes: make([]dashboardOfflineNode, 0),
	}
	for _, client := range clientList {
		if _, online := onlineSet[client.UUID]; online {
			summary.Online++
			continue
		}
		node := dashboardOfflineNode{UUID: client.UUID, Name: client.Name, Region: client.Region}
		if report := latest[client.UUID]; report != nil && !report.UpdatedAt.IsZero() {
			lastSeen := report.UpdatedAt.UTC()
			node.LastSeen = &lastSeen
		}
		summary.OfflineNodes = append(summary.OfflineNodes, node)
	}
	summary.Offline = len(summary.OfflineNodes)
	sort.SliceStable(summary.OfflineNodes, func(i, j int) bool {
		left, right := summary.OfflineNodes[i], summary.OfflineNodes[j]
		if left.LastSeen == nil {
			return right.LastSeen != nil
		}
		if right.LastSeen == nil {
			return false
		}
		return left.LastSeen.Before(*right.LastSeen)
	})
	return summary
}

func loadDashboardTraffic(ctx context.Context, clientList []models.Client, now time.Time, rankingLimit int) (dashboardTrafficSummary, error) {
	return loadDashboardTrafficFrom(ctx, dbcore.GetDBInstance(), clientList, now, rankingLimit, metricstore.TelemetryEnabled(), dashboardapp.LoadTrafficTrendSnapshot)
}

type dashboardTrendLoader func(context.Context, []string, time.Time, time.Duration, time.Duration) (*dashboardapp.TrafficTrendResult, error)

func loadDashboardTrafficFrom(ctx context.Context, db *gorm.DB, clientList []models.Client, now time.Time, rankingLimit int, telemetry bool, loadTrend dashboardTrendLoader) (dashboardTrafficSummary, error) {
	today := trafficledger.BeijingDay(now)
	start := today.AddDate(0, 0, -(trafficledger.DashboardHistoryDays - 1))
	var rows []models.TrafficDailyLedger
	todayUsage := make(map[string]trafficledger.Usage, len(clientList))
	adjustments := map[string]trafficledger.SignedUsage{}
	var todayErr error
	var through *time.Time
	clientIDs := make([]string, 0, len(clientList))
	for _, client := range clientList {
		clientIDs = append(clientIDs, client.UUID)
	}
	// Historical days, today's ledger and calibration share one business snapshot.
	// Trend precision and availability never participate in billing success.
	err := trafficledger.WithTelemetryReadSnapshot(ctx, db, func(tx *gorm.DB) error {
		if err := tx.Select("client", "day", "up_bytes", "down_bytes").Where("day >= ? AND day < ?", start.Format(time.DateOnly), today.Format(time.DateOnly)).Find(&rows).Error; err != nil {
			return fmt.Errorf("read dashboard traffic ledger: %w", err)
		}
		var err error
		adjustments, err = trafficledger.DailyAdjustments(ctx, tx, start, today.AddDate(0, 0, 1))
		if err != nil {
			return fmt.Errorf("read dashboard traffic calibration: %w", err)
		}
		if !telemetry {
			return nil
		}
		// A savepoint contains driver statement failures, retaining settled days.
		todayErr = tx.Transaction(func(dayTx *gorm.DB) error {
			var current []models.TrafficDailyLedger
			if err := dayTx.Select("client, up_bytes, down_bytes").Where("day = ?", today.Format(time.DateOnly)).Find(&current).Error; err != nil {
				return err
			}
			for _, row := range current {
				todayUsage[row.Client] = trafficledger.Usage{Up: row.UpBytes, Down: row.DownBytes}
			}
			for offset := 0; offset < len(clientIDs); offset += 128 {
				ids := clientIDs[offset:min(offset+128, len(clientIDs))]
				var states []models.TelemetryTrafficState
				if err := dayTx.Select("client, last_observed").Where("client IN ?", ids).Find(&states).Error; err != nil {
					return err
				}
				if len(states) != len(ids) {
					return fmt.Errorf("traffic accounting history is incomplete")
				}
				for _, state := range states {
					at := time.Unix(0, state.LastObserved).UTC()
					if through == nil || at.Before(*through) {
						through = &at
					}
				}
			}
			return nil
		})
		return nil
	})
	if err != nil {
		return dashboardTrafficSummary{}, err
	}
	if !telemetry {
		todayUsage, _, todayErr = trafficledger.MetricUsageByHourBatch(ctx, clientIDs, today.UTC(), now.UTC())
	}
	trend, trendErr := loadTrend(ctx, clientIDs, now, dashboardapp.DefaultTrafficTrendWindow, dashboardapp.DefaultTrafficTrendInterval)
	var buckets []dashboardapp.TrafficTrendBucket
	if trendErr == nil {
		buckets = trend.Buckets
	}
	if todayErr != nil {
		todayUsage = nil
	}
	summary := summarizeDashboardTraffic(clientList, rows, todayUsage, buckets, adjustments, now, rankingLimit)
	summary.AccountingThrough = through
	if todayErr != nil {
		summary.TodayError = todayErr.Error()
		summary.RankingError = todayErr.Error()
		summary.TodayUp = 0
		summary.TodayDown = 0
		summary.TodayBillable = 0
		summary.Ranking = nil
		for i := range summary.Daily {
			if summary.Daily[i].Day == today.Format(time.DateOnly) {
				available := false
				summary.Daily[i].Available = &available
				summary.Daily[i].Up = 0
				summary.Daily[i].Down = 0
				summary.Daily[i].Billable = 0
			}
		}
	}
	if trendErr != nil {
		summary.TrendError = trendErr.Error()
	} else {
		summary.TrendCoverage = trend.Coverage
		summary.CatchingUp = trend.CatchingUp
		if trend.Coverage == "unavailable" {
			summary.TrendError = trend.Warning
		}
	}
	return summary, nil
}

func summarizeDashboardTraffic(clientList []models.Client, rows []models.TrafficDailyLedger, todayUsage map[string]trafficledger.Usage, trendBuckets []dashboardapp.TrafficTrendBucket, adjustments map[string]trafficledger.SignedUsage, now time.Time, rankingLimit int) dashboardTrafficSummary {
	today := trafficledger.BeijingDay(now)
	start := today.AddDate(0, 0, -(trafficledger.DashboardHistoryDays - 1))
	clientsByID := make(map[string]models.Client, len(clientList))
	for _, client := range clientList {
		clientsByID[client.UUID] = client
	}

	daysByKey := make(map[string]*dashboardTrafficDay, trafficledger.DashboardHistoryDays)
	summary := dashboardTrafficSummary{
		Daily:  make([]dashboardTrafficDay, 0, trafficledger.DashboardHistoryDays),
		Hourly: make([]dashboardTrafficBucket, len(trendBuckets)),
	}
	for index, bucket := range trendBuckets {
		summary.Hourly[index] = dashboardTrafficBucket{
			Hour: bucket.Start.In(trafficledger.BeijingLocation).Format("01-02 15:04"),
			Up:   bucket.Up,
			Down: bucket.Down,
		}
	}
	for day := start; !day.After(today); day = day.AddDate(0, 0, 1) {
		key := day.Format(time.DateOnly)
		summary.Daily = append(summary.Daily, dashboardTrafficDay{Day: key})
		daysByKey[key] = &summary.Daily[len(summary.Daily)-1]
	}

	seen := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		client, ok := clientsByID[row.Client]
		day := daysByKey[row.Day]
		if !ok || day == nil {
			continue
		}
		usage := trafficledger.ApplyAdjustment(
			trafficledger.Usage{Up: row.UpBytes, Down: row.DownBytes},
			adjustments[row.Client+"\x00"+row.Day],
		)
		day.Up += usage.Up
		day.Down += usage.Down
		if client.Price > 0 {
			day.Billable += trafficledger.BillableUsage(client.TrafficLimitType, usage.Up, usage.Down)
		}
		seen[row.Client+"\x00"+row.Day] = struct{}{}
	}

	todayKey := today.Format(time.DateOnly)
	todayDay := daysByKey[todayKey]
	for _, client := range clientList {
		adjustment := adjustments[client.UUID+"\x00"+todayKey]
		usage := trafficledger.ApplyAdjustment(todayUsage[client.UUID], adjustment)
		todayDay.Up += usage.Up
		todayDay.Down += usage.Down
		billable := int64(0)
		if client.Price > 0 {
			billable = trafficledger.BillableUsage(client.TrafficLimitType, usage.Up, usage.Down)
		}
		todayDay.Billable += billable
		summary.TodayUp += usage.Up
		summary.TodayDown += usage.Down
		summary.TodayBillable += billable
		name := strings.TrimSpace(client.Name)
		if name == "" {
			name = client.UUID
		}
		rankingBillable := trafficledger.BillableUsage(client.TrafficLimitType, usage.Up, usage.Down)
		if rankingBillable > 0 {
			summary.Ranking = dashboardTopTraffic(summary.Ranking, dashboardTrafficRankItem{
				UUID: client.UUID, Name: name, Up: usage.Up, Down: usage.Down, Billable: rankingBillable,
			}, rankingLimit)
		}
	}

	expectedRows := len(clientList) * (trafficledger.DashboardHistoryDays - 1)
	summary.HistoryReady = len(seen) == expectedRows
	return summary
}

func dashboardTopTraffic(top []dashboardTrafficRankItem, item dashboardTrafficRankItem, limit int) []dashboardTrafficRankItem {
	if !dashboardRankingLimitAllowed(limit) {
		limit = 5
	}
	insertAt := len(top)
	for index, current := range top {
		if item.Billable > current.Billable || (item.Billable == current.Billable && item.Name < current.Name) {
			insertAt = index
			break
		}
	}
	if insertAt >= limit {
		return top
	}
	if len(top) < limit {
		top = append(top, dashboardTrafficRankItem{})
	}
	copy(top[insertAt+1:], top[insertAt:len(top)-1])
	top[insertAt] = item
	return top
}
