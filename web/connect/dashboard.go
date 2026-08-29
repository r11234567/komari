package connectapi

// DashboardService is the production reader for the administration dashboard.
// It reuses the aggregation behind the legacy admin:getDashboard* methods so the
// typed and legacy views can never disagree; only unconverted themes still read
// the legacy JSON over /api/rpc2.
//
// The jsonrpc dependency is transitional: the aggregation moves to web/dashboard
// in the extraction pass and only the import below changes.

import (
	"context"
	"errors"
	"time"

	"connectrpc.com/connect"
	jsonRpc "github.com/komari-monitor/komari/web/rpc/jsonrpc"
	adminv1 "github.com/r11234567/komari-proto/gen/go/komari/admin/v1"
	adminv1connect "github.com/r11234567/komari-proto/gen/go/komari/admin/v1/adminv1connect"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type dashboardService struct {
	adminv1connect.UnimplementedDashboardServiceHandler
}

func (s *dashboardService) GetDashboardSummary(ctx context.Context, req *connect.Request[adminv1.GetDashboardSummaryRequest]) (*connect.Response[adminv1.GetDashboardSummaryResponse], error) {
	release, err := acquireMetricReadSlot(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	sections := make([]jsonRpc.DashboardSection, 0, len(req.Msg.Sections))
	for _, section := range req.Msg.Sections {
		if selector, ok := summarySectionSelector(section); ok {
			sections = append(sections, selector)
		}
	}
	result, err := jsonRpc.DashboardSummaryAggregate(ctx, sections, int(req.Msg.RankingLimit))
	if err != nil {
		return nil, connectError(connect.CodeInternal, err)
	}
	requested := requestedSummarySections(req.Msg.Sections)
	summary := &adminv1.DashboardSummary{GeneratedAt: timestamppb.New(result.GeneratedAt)}
	if requested[adminv1.DashboardSection_DASHBOARD_SECTION_SERVERS] {
		summary.Servers = serversToProto(result.Servers)
	}
	if requested[adminv1.DashboardSection_DASHBOARD_SECTION_RESOURCES] {
		summary.Resources = resourcesToProto(result.Resources)
	}
	if requested[adminv1.DashboardSection_DASHBOARD_SECTION_STORAGE] {
		summary.Database = databaseToProto(result.Database)
		summary.Storage = storageToProto(result.Storage)
	}
	if requested[adminv1.DashboardSection_DASHBOARD_SECTION_RETURN_ROUTE] {
		summary.ReturnRoute = returnRouteToProto(result.ReturnRoute)
	}
	if requested[adminv1.DashboardSection_DASHBOARD_SECTION_ALERTS] {
		summary.Alerts = alertsToProto(result.Alerts)
	}
	return connect.NewResponse(&adminv1.GetDashboardSummaryResponse{Summary: summary}), nil
}

func (s *dashboardService) GetDashboardCharts(ctx context.Context, req *connect.Request[adminv1.GetDashboardChartsRequest]) (*connect.Response[adminv1.GetDashboardChartsResponse], error) {
	release, err := acquireMetricReadSlot(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	charts := make([]jsonRpc.DashboardChart, 0, len(req.Msg.Charts))
	for _, chart := range req.Msg.Charts {
		if selector, ok := chartSelector(chart); ok {
			charts = append(charts, selector)
		}
	}
	result := jsonRpc.DashboardChartsAggregate(ctx, charts, int(req.Msg.RankingLimit))
	requested := requestedCharts(req.Msg.Charts)
	series := &adminv1.DashboardCharts{GeneratedAt: timestamppb.New(result.GeneratedAt)}
	if requested[adminv1.DashboardChart_DASHBOARD_CHART_TRAFFIC] {
		series.Traffic = trafficToProto(result.Traffic)
	}
	// Latency and jitter share one aggregate; either selector publishes it.
	if requested[adminv1.DashboardChart_DASHBOARD_CHART_LATENCY] || requested[adminv1.DashboardChart_DASHBOARD_CHART_LATENCY_JITTER] {
		series.Latency = latencyToProto(result.Latency)
	}
	if requested[adminv1.DashboardChart_DASHBOARD_CHART_PACKET_LOSS] {
		series.PacketLoss = packetLossToProto(result.PacketLoss)
	}
	return connect.NewResponse(&adminv1.GetDashboardChartsResponse{Charts: series}), nil
}

func (s *dashboardService) ListDashboardAlertItems(_ context.Context, req *connect.Request[adminv1.ListDashboardAlertItemsRequest]) (*connect.Response[adminv1.ListDashboardAlertItemsResponse], error) {
	kind, ok := alertKindName(req.Msg.Kind)
	if !ok {
		return nil, connectError(connect.CodeInvalidArgument, errors.New("unsupported dashboard alert kind"))
	}
	result, err := jsonRpc.DashboardAlertItems(kind)
	switch {
	case errors.Is(err, jsonRpc.ErrDashboardAlertKind):
		return nil, connectError(connect.CodeInvalidArgument, err)
	case err != nil:
		return nil, connectError(connect.CodeInternal, err)
	}
	items := make([]*adminv1.DashboardAlertItem, 0, len(result.Items))
	for _, item := range result.Items {
		items = append(items, alertItemToProto(item))
	}
	return connect.NewResponse(&adminv1.ListDashboardAlertItemsResponse{
		Kind:        req.Msg.Kind,
		Items:       items,
		GeneratedAt: timestamppb.New(result.GeneratedAt),
	}), nil
}

func summarySectionSelector(section adminv1.DashboardSection) (jsonRpc.DashboardSection, bool) {
	switch section {
	case adminv1.DashboardSection_DASHBOARD_SECTION_SERVERS:
		return jsonRpc.DashboardSectionServersSelector, true
	case adminv1.DashboardSection_DASHBOARD_SECTION_RESOURCES:
		return jsonRpc.DashboardSectionResourcesSelector, true
	case adminv1.DashboardSection_DASHBOARD_SECTION_STORAGE:
		return jsonRpc.DashboardSectionStorageSelector, true
	case adminv1.DashboardSection_DASHBOARD_SECTION_RETURN_ROUTE:
		return jsonRpc.DashboardSectionReturnRouteSelector, true
	case adminv1.DashboardSection_DASHBOARD_SECTION_ALERTS:
		return jsonRpc.DashboardSectionAlertsSelector, true
	default:
		return 0, false
	}
}

func chartSelector(chart adminv1.DashboardChart) (jsonRpc.DashboardChart, bool) {
	switch chart {
	case adminv1.DashboardChart_DASHBOARD_CHART_TRAFFIC:
		return jsonRpc.DashboardChartTrafficSelector, true
	case adminv1.DashboardChart_DASHBOARD_CHART_LATENCY:
		return jsonRpc.DashboardChartLatencySelector, true
	case adminv1.DashboardChart_DASHBOARD_CHART_LATENCY_JITTER:
		return jsonRpc.DashboardChartLatencyJitterSelector, true
	case adminv1.DashboardChart_DASHBOARD_CHART_PACKET_LOSS:
		return jsonRpc.DashboardChartPacketLossSelector, true
	default:
		return 0, false
	}
}

// requestedSummarySections mirrors the aggregate's "empty selection means all"
// rule so an unrequested section stays unset instead of reporting zeroes.
func requestedSummarySections(sections []adminv1.DashboardSection) map[adminv1.DashboardSection]bool {
	result := make(map[adminv1.DashboardSection]bool, len(sections))
	for _, section := range sections {
		if _, ok := summarySectionSelector(section); ok {
			result[section] = true
		}
	}
	if len(result) == 0 {
		return map[adminv1.DashboardSection]bool{
			adminv1.DashboardSection_DASHBOARD_SECTION_SERVERS:      true,
			adminv1.DashboardSection_DASHBOARD_SECTION_RESOURCES:    true,
			adminv1.DashboardSection_DASHBOARD_SECTION_STORAGE:      true,
			adminv1.DashboardSection_DASHBOARD_SECTION_RETURN_ROUTE: true,
			adminv1.DashboardSection_DASHBOARD_SECTION_ALERTS:       true,
		}
	}
	return result
}

func requestedCharts(charts []adminv1.DashboardChart) map[adminv1.DashboardChart]bool {
	result := make(map[adminv1.DashboardChart]bool, len(charts))
	for _, chart := range charts {
		if _, ok := chartSelector(chart); ok {
			result[chart] = true
		}
	}
	if len(result) == 0 {
		return map[adminv1.DashboardChart]bool{
			adminv1.DashboardChart_DASHBOARD_CHART_TRAFFIC:        true,
			adminv1.DashboardChart_DASHBOARD_CHART_LATENCY:        true,
			adminv1.DashboardChart_DASHBOARD_CHART_LATENCY_JITTER: true,
			adminv1.DashboardChart_DASHBOARD_CHART_PACKET_LOSS:    true,
		}
	}
	return result
}

func alertKindName(kind adminv1.DashboardAlertKind) (string, bool) {
	switch kind {
	case adminv1.DashboardAlertKind_DASHBOARD_ALERT_KIND_OFFLINE:
		return "offline", true
	case adminv1.DashboardAlertKind_DASHBOARD_ALERT_KIND_RESOURCE:
		return "resource", true
	case adminv1.DashboardAlertKind_DASHBOARD_ALERT_KIND_LATENCY_LOSS:
		return "latency_loss", true
	case adminv1.DashboardAlertKind_DASHBOARD_ALERT_KIND_TRAFFIC:
		return "traffic", true
	case adminv1.DashboardAlertKind_DASHBOARD_ALERT_KIND_RETURN_ROUTE:
		return "return_route", true
	case adminv1.DashboardAlertKind_DASHBOARD_ALERT_KIND_BILLING:
		return "billing", true
	default:
		return "", false
	}
}

func alertKindValue(kind string) adminv1.DashboardAlertKind {
	switch kind {
	case "offline":
		return adminv1.DashboardAlertKind_DASHBOARD_ALERT_KIND_OFFLINE
	case "resource":
		return adminv1.DashboardAlertKind_DASHBOARD_ALERT_KIND_RESOURCE
	case "latency_loss":
		return adminv1.DashboardAlertKind_DASHBOARD_ALERT_KIND_LATENCY_LOSS
	case "traffic":
		return adminv1.DashboardAlertKind_DASHBOARD_ALERT_KIND_TRAFFIC
	case "return_route":
		return adminv1.DashboardAlertKind_DASHBOARD_ALERT_KIND_RETURN_ROUTE
	case "billing":
		return adminv1.DashboardAlertKind_DASHBOARD_ALERT_KIND_BILLING
	default:
		return adminv1.DashboardAlertKind_DASHBOARD_ALERT_KIND_UNSPECIFIED
	}
}

func serversToProto(value jsonRpc.DashboardServers) *adminv1.DashboardServers {
	nodes := make([]*adminv1.DashboardOfflineNode, 0, len(value.OfflineNodes))
	for _, node := range value.OfflineNodes {
		nodes = append(nodes, &adminv1.DashboardOfflineNode{
			Uuid:     node.UUID,
			Name:     node.Name,
			Region:   node.Region,
			LastSeen: optionalTimestamp(node.LastSeen),
		})
	}
	return &adminv1.DashboardServers{
		Total:        int32(value.Total),
		Online:       int32(value.Online),
		Offline:      int32(value.Offline),
		OfflineNodes: nodes,
	}
}

func resourcesToProto(value jsonRpc.DashboardResources) *adminv1.DashboardResources {
	return &adminv1.DashboardResources{
		Cpu:    resourceRankToProto(value.CPU),
		Memory: resourceRankToProto(value.Memory),
		Disk:   resourceRankToProto(value.Disk),
	}
}

func resourceRankToProto(items []jsonRpc.DashboardResourceRankItem) []*adminv1.DashboardResourceRankItem {
	result := make([]*adminv1.DashboardResourceRankItem, 0, len(items))
	for _, item := range items {
		result = append(result, &adminv1.DashboardResourceRankItem{
			Uuid:          item.UUID,
			Name:          item.Name,
			CpuPercent:    item.CPU,
			MemoryPercent: item.Memory,
			DiskPercent:   item.Disk,
			DetailUrl:     item.DetailURL,
		})
	}
	return result
}

func databaseToProto(value jsonRpc.DashboardDatabase) *adminv1.DashboardDatabase {
	return &adminv1.DashboardDatabase{
		Type:            value.Type,
		TotalBytes:      value.Size,
		Main:            databaseStoreToProto(value.Main),
		Monitoring:      databaseStoreToProto(value.Monitoring),
		LocalTotalBytes: value.LocalTotal,
	}
}

func databaseStoreToProto(value jsonRpc.DashboardDatabaseStore) *adminv1.DashboardDatabaseStore {
	result := &adminv1.DashboardDatabaseStore{
		Driver:    value.Driver,
		Location:  value.Location,
		SizeBytes: value.Size,
		Error:     value.Error,
		Action:    value.Action,
	}
	if value.Files != nil {
		result.Files = &adminv1.DashboardDatabaseFiles{
			DatabaseBytes: value.Files.Database,
			WalBytes:      value.Files.WAL,
			ShmBytes:      value.Files.SHM,
		}
	}
	return result
}

func storageToProto(value jsonRpc.DashboardStorage) *adminv1.DashboardStorage {
	return &adminv1.DashboardStorage{
		DatabaseFileBytes: value.DatabaseFiles,
		WalBytes:          value.WAL,
		ShmBytes:          value.SHM,
		RetentionDays:     int32(value.RetentionDays),
		LastCompactedAt:   optionalTimestamp(value.LastCompactedAt),
	}
}

func returnRouteToProto(value jsonRpc.DashboardReturnRoute) *adminv1.DashboardReturnRoute {
	result := &adminv1.DashboardReturnRoute{
		Tasks:        value.Tasks,
		Active:       value.Active,
		Healthy:      value.Healthy,
		Switched:     value.Switched,
		Abnormal:     value.Abnormal,
		RecentEvents: value.RecentEvents,
		Error:        value.Error,
	}
	if value.LatestEvent != nil {
		result.LatestEvent = &adminv1.DashboardReturnRouteEvent{
			Id:           uint64(value.LatestEvent.Id),
			TaskName:     value.LatestEvent.TaskName,
			NodeName:     value.LatestEvent.NodeName,
			ExpectedLine: value.LatestEvent.ExpectedLine,
			FromLine:     value.LatestEvent.FromLine,
			ToLine:       value.LatestEvent.ToLine,
			Kind:         value.LatestEvent.Kind,
			OccurredAt:   timestamppb.New(value.LatestEvent.OccurredAt),
		}
	}
	return result
}

func alertsToProto(value jsonRpc.DashboardAlerts) *adminv1.DashboardAlerts {
	return &adminv1.DashboardAlerts{
		Resource:    alertSummaryToProto(value.Resource),
		Offline:     alertSummaryToProto(value.Offline),
		LatencyLoss: alertSummaryToProto(value.LatencyLoss),
		Traffic:     alertSummaryToProto(value.Traffic),
		ReturnRoute: alertSummaryToProto(value.ReturnRoute),
		Billing:     alertSummaryToProto(value.Billing),
	}
}

func alertSummaryToProto(value jsonRpc.DashboardAlertSummary) *adminv1.DashboardAlertSummary {
	result := &adminv1.DashboardAlertSummary{
		Current:        int32(value.Current),
		AffectedNodes:  int32(value.AffectedNodes),
		RecoveredToday: int32(value.RecoveredToday),
		Error:          value.Error,
	}
	if value.LatestAlert != nil {
		result.LatestAlert = &adminv1.DashboardAlertItem{
			Title:      value.LatestAlert.Title,
			NodeUuid:   value.LatestAlert.NodeUUID,
			NodeName:   value.LatestAlert.NodeName,
			TaskId:     uint32(value.LatestAlert.TaskID),
			TaskName:   value.LatestAlert.TaskName,
			OccurredAt: optionalTimestamp(value.LatestAlert.OccurredAt),
			DueAt:      optionalTimestamp(value.LatestAlert.DueAt),
		}
	}
	return result
}

func alertItemToProto(value jsonRpc.DashboardAlertItem) *adminv1.DashboardAlertItem {
	return &adminv1.DashboardAlertItem{
		Kind:       alertKindValue(value.Kind),
		Title:      value.Title,
		NodeUuid:   value.NodeUUID,
		NodeName:   value.NodeName,
		TaskId:     uint32(value.TaskID),
		TaskName:   value.TaskName,
		OccurredAt: optionalTimestamp(value.OccurredAt),
		DueAt:      optionalTimestamp(value.DueAt),
	}
}

func trafficToProto(value jsonRpc.DashboardTraffic) *adminv1.DashboardTraffic {
	hourly := make([]*adminv1.DashboardTrafficBucket, 0, len(value.Hourly))
	for _, bucket := range value.Hourly {
		hourly = append(hourly, &adminv1.DashboardTrafficBucket{
			Label:         bucket.Hour,
			UploadBytes:   bucket.Up,
			DownloadBytes: bucket.Down,
		})
	}
	daily := make([]*adminv1.DashboardTrafficDay, 0, len(value.Daily))
	for _, day := range value.Daily {
		daily = append(daily, &adminv1.DashboardTrafficDay{
			Day:           day.Day,
			UploadBytes:   day.Up,
			DownloadBytes: day.Down,
			BillableBytes: day.Billable,
		})
	}
	ranking := make([]*adminv1.DashboardTrafficRankItem, 0, len(value.Ranking))
	for _, item := range value.Ranking {
		ranking = append(ranking, &adminv1.DashboardTrafficRankItem{
			Uuid:          item.UUID,
			Name:          item.Name,
			UploadBytes:   item.Up,
			DownloadBytes: item.Down,
			BillableBytes: item.Billable,
			DetailUrl:     item.DetailURL,
		})
	}
	return &adminv1.DashboardTraffic{
		TodayUploadBytes:   value.TodayUp,
		TodayDownloadBytes: value.TodayDown,
		TodayBillableBytes: value.TodayBillable,
		Hourly:             hourly,
		Daily:              daily,
		Ranking:            ranking,
		HistoryReady:       value.HistoryReady,
		Error:              value.Error,
	}
}

func latencyToProto(value jsonRpc.DashboardLatency) *adminv1.DashboardLatency {
	points := make([]*adminv1.DashboardLatencyPoint, 0, len(value.Points))
	for _, point := range value.Points {
		points = append(points, &adminv1.DashboardLatencyPoint{
			Time:      timestamppb.New(point.Time),
			AverageMs: point.Average,
		})
	}
	ranking := make([]*adminv1.DashboardLatencyRankItem, 0, len(value.Ranking))
	for _, item := range value.Ranking {
		ranking = append(ranking, &adminv1.DashboardLatencyRankItem{
			Uuid:      item.UUID,
			Name:      item.Name,
			AverageMs: item.Average,
			TaskId:    uint32(item.TaskID),
			DetailUrl: item.DetailURL,
		})
	}
	jitter := make([]*adminv1.DashboardLatencyJitterRankItem, 0, len(value.JitterRanking))
	for _, item := range value.JitterRanking {
		jitter = append(jitter, &adminv1.DashboardLatencyJitterRankItem{
			Uuid:       item.UUID,
			Name:       item.Name,
			PreviousMs: item.Previous,
			CurrentMs:  item.Current,
			DeltaMs:    item.Delta,
			TaskId:     uint32(item.TaskID),
			DetailUrl:  item.DetailURL,
		})
	}
	return &adminv1.DashboardLatency{
		AverageMs:     value.Average,
		Targets:       int32(value.Targets),
		Points:        points,
		Ranking:       ranking,
		JitterRanking: jitter,
		JitterError:   value.JitterError,
		Error:         value.Error,
	}
}

func packetLossToProto(value jsonRpc.DashboardPacketLoss) *adminv1.DashboardPacketLoss {
	ranking := make([]*adminv1.DashboardPacketLossRankItem, 0, len(value.Ranking))
	for _, item := range value.Ranking {
		ranking = append(ranking, &adminv1.DashboardPacketLossRankItem{
			Uuid:      item.UUID,
			Name:      item.Name,
			TaskId:    uint32(item.TaskID),
			TaskName:  item.TaskName,
			LossRate:  item.LossRate,
			Lost:      int32(item.Lost),
			Total:     int32(item.Total),
			Valid:     int32(item.Valid),
			DetailUrl: item.DetailURL,
		})
	}
	return &adminv1.DashboardPacketLoss{
		WindowMinutes: int32(value.WindowMinutes),
		Ranking:       ranking,
		Error:         value.Error,
	}
}

func optionalTimestamp(value *time.Time) *timestamppb.Timestamp {
	if value == nil {
		return nil
	}
	return timestamppb.New(*value)
}
