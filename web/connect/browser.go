package connectapi

import (
	"context"
	"errors"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/database"
	"github.com/komari-monitor/komari/database/clients"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/pkg/config"
	"github.com/komari-monitor/komari/pkg/rpc"
	legacyv1 "github.com/komari-monitor/komari/protocol/v1"
	"github.com/komari-monitor/komari/utils"
	agent_runtime "github.com/komari-monitor/komari/web/agent"
	browserv1 "github.com/r11234567/komari-proto/gen/go/komari/browser/v1"
	browserv1connect "github.com/r11234567/komari-proto/gen/go/komari/browser/v1/browserv1connect"
	reportv1 "github.com/r11234567/komari-proto/gen/go/komari/report/v1"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type browserService struct {
	browserv1connect.UnimplementedBrowserServiceHandler
}

func (s *browserService) GetPublicInfo(ctx context.Context, _ *connect.Request[browserv1.GetPublicInfoRequest]) (*connect.Response[browserv1.GetPublicInfoResponse], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	info, err := database.GetPublicInfo()
	if err != nil {
		return nil, connectError(connect.CodeInternal, err)
	}
	themeSettings, err := structValue(info, "theme_settings")
	if err != nil {
		return nil, connectError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&browserv1.GetPublicInfoResponse{
		SiteName:               stringValue(info, "sitename"),
		SiteDescription:        stringValue(info, "description"),
		Version:                utils.CurrentVersion,
		DefaultTheme:           stringValue(info, "theme"),
		CorsOriginCheckEnabled: boolValue(info, "cors_origin_check_enabled"),
		CustomBody:             stringValue(info, "custom_body"),
		CustomHead:             stringValue(info, "custom_head"),
		DisablePasswordLogin:   boolValue(info, "disable_password_login"),
		OauthProvider:          stringValue(info, "oauth_provider"),
		OauthEnabled:           boolValue(info, "oauth_enable"),
		MetricRetentionDays:    uint32(max(intValue(info, "record_preserve_time")/24, 0)),
		PrivateSite:            boolValue(info, "private_site") && !isTemporaryShare(ctx),
		ThemeSettings:          themeSettings,
		VisitorAuditEnabled:    boolValue(info, "visitor_audit_enabled"),
	}), nil
}

func isTemporaryShare(ctx context.Context) bool {
	meta := rpc.MetaFromContext(ctx)
	return meta != nil && meta.TempShareValid
}

func (s *browserService) ListAgents(ctx context.Context, req *connect.Request[browserv1.ListAgentsRequest]) (*connect.Response[browserv1.ListAgentsResponse], error) {
	list, err := clients.GetAllClientBasicInfo()
	if err != nil {
		return nil, connectError(connect.CodeInternal, err)
	}
	latest := agent_runtime.GetLatestReport()
	online := stringSet(agent_runtime.GetAllOnlineUUIDs())
	meta := rpc.MetaFromContext(ctx)
	isAdmin := meta != nil && meta.Principal != nil && meta.Principal.HasRole(rpc.RoleAdmin)
	showGuestIP, _ := config.GetAs[bool](config.SendIpAddrToGuestKey, false)
	filter := stringSet(req.Msg.AgentIds)
	search := strings.ToLower(strings.TrimSpace(req.Msg.Search))
	result := make([]*browserv1.AgentSummary, 0, len(list))
	for _, item := range list {
		if item.Hidden && !isAdmin {
			continue
		}
		if len(filter) > 0 && !filter[item.UUID] {
			continue
		}
		if search != "" && !strings.Contains(strings.ToLower(item.Name+" "+item.UUID), search) {
			continue
		}
		result = append(
			result,
			browserSummary(item, latest[item.UUID], online[item.UUID], isAdmin, showGuestIP),
		)
	}
	return connect.NewResponse(&browserv1.ListAgentsResponse{Agents: result}), nil
}

func (s *browserService) GetAgent(ctx context.Context, req *connect.Request[browserv1.GetAgentRequest]) (*connect.Response[browserv1.GetAgentResponse], error) {
	if strings.TrimSpace(req.Msg.AgentId) == "" {
		return nil, connectError(connect.CodeInvalidArgument, errors.New("agent ID is required"))
	}
	item, err := clients.GetClientByUUID(req.Msg.AgentId)
	if err != nil {
		return nil, connectError(connect.CodeNotFound, errors.New("agent not found"))
	}
	meta := rpc.MetaFromContext(ctx)
	isAdmin := meta != nil && meta.Principal != nil && meta.Principal.HasRole(rpc.RoleAdmin)
	showGuestIP, _ := config.GetAs[bool](config.SendIpAddrToGuestKey, false)
	if item.Hidden && !isAdmin {
		return nil, connectError(connect.CodeNotFound, errors.New("agent not found"))
	}
	report := agent_runtime.GetLatestReport()[item.UUID]
	online := stringSet(agent_runtime.GetAllOnlineUUIDs())
	return connect.NewResponse(&browserv1.GetAgentResponse{
		Agent: browserSummary(
			item,
			report,
			online[item.UUID],
			isAdmin,
			showGuestIP,
		),
		LatestReport: legacyReportToProto(item, report, isAdmin),
	}), nil
}

func (s *browserService) GetThemeContract(context.Context, *connect.Request[browserv1.GetThemeContractRequest]) (*connect.Response[browserv1.GetThemeContractResponse], error) {
	return connect.NewResponse(&browserv1.GetThemeContractResponse{
		SchemaVersion:          1,
		ManifestName:           "komari-theme.json",
		ConnectBasePath:        "/komari.browser.v1.BrowserService/",
		LegacyJsonRpcAvailable: true,
	}), nil
}

func browserSummary(client models.Client, report *legacyv1.Report, online, includePrivate, showGuestIP bool) *browserv1.AgentSummary {
	result := &browserv1.AgentSummary{
		AgentId:   client.UUID,
		Name:      client.Name,
		Status:    browserStatus(online),
		BasicInfo: browserBasicInfo(client, includePrivate, showGuestIP),
	}
	if report != nil {
		result.LastSeen = timestamppb.New(report.UpdatedAt)
		result.CpuPercent = report.CPU.Usage
		if report.Ram.Total > 0 {
			result.MemoryPercent = float64(report.Ram.Used) * 100 / float64(report.Ram.Total)
		}
	}
	return result
}

func browserBasicInfo(client models.Client, includePrivate, showGuestIP bool) *browserv1.AgentBasicInfo {
	trafficLimit := client.EffectiveTrafficLimit
	if trafficLimit == 0 && client.TrafficLimit > 0 {
		trafficLimit = client.TrafficLimit
	}
	trafficLimitType := client.EffectiveTrafficType
	if trafficLimitType == "" {
		trafficLimitType = client.TrafficLimitType
	}
	result := &browserv1.AgentBasicInfo{
		CpuName:           client.CpuName,
		Virtualization:    client.Virtualization,
		Architecture:      client.Arch,
		CpuCores:          uint32(max(client.CpuCores, 0)),
		Os:                client.OS,
		KernelVersion:     client.KernelVersion,
		GpuName:           client.GpuName,
		Region:            client.Region,
		MemoryTotalBytes:  uint64(max(client.MemTotal, 0)),
		SwapTotalBytes:    uint64(max(client.SwapTotal, 0)),
		DiskTotalBytes:    uint64(max(client.DiskTotal, 0)),
		Weight:            int32(client.Weight),
		Price:             client.Price,
		Tags:              client.Tags,
		BillingCycleDays:  uint32(max(client.BillingCycle, 0)),
		Currency:          client.Currency,
		Group:             client.Group,
		TrafficLimitBytes: uint64(max(trafficLimit, 0)),
		TrafficLimitType:  trafficLimitType,
		CreatedAt:         timestamppb.New(client.CreatedAt),
		UpdatedAt:         timestamppb.New(client.UpdatedAt),
	}
	if client.ExpiredAt != nil {
		result.ExpiresAt = timestamppb.New(*client.ExpiredAt)
	}
	if includePrivate {
		result.AgentVersion = client.Version
		result.Ipv4, result.Ipv6 = client.IPv4, client.IPv6
	} else if showGuestIP {
		result.Ipv4, result.Ipv6 = maskGuestIP(client.IPv4, client.IPv6)
	}
	return result
}

func maskGuestIP(ipv4, ipv6 string) (string, string) {
	if ipv4 != "" {
		ipv4 = strings.Split(ipv4, ".")[0] + ".*.*.*"
	}
	if ipv6 != "" {
		ipv6 = strings.Split(ipv6, ":")[0] + ":*:*:*:*:*:*:*"
	}
	return ipv4, ipv6
}

func legacyReportToProto(client models.Client, report *legacyv1.Report, includePrivate bool) *reportv1.AgentReport {
	if report == nil {
		return nil
	}
	result := &reportv1.AgentReport{
		AgentId:    client.UUID,
		ObservedAt: timestamppb.New(report.UpdatedAt),
		System: &reportv1.SystemInfo{
			Hostname:         client.Name,
			Os:               client.OS,
			KernelVersion:    client.KernelVersion,
			Architecture:     client.Arch,
			CpuCount:         uint32(max(client.CpuCores, 0)),
			MemoryTotalBytes: uint64(max(report.Ram.Total, 0)),
			Uptime:           durationpb.New(time.Duration(report.Uptime) * time.Second),
		},
		Resources: &reportv1.ResourceUsage{
			CpuPercent:           report.CPU.Usage,
			MemoryUsedBytes:      uint64(max(report.Ram.Used, 0)),
			MemoryAvailableBytes: uint64(max(report.Ram.Total-report.Ram.Used, 0)),
			SwapUsedBytes:        uint64(max(report.Swap.Used, 0)),
			SwapTotalBytes:       uint64(max(report.Swap.Total, 0)),
			LoadAverage:          []float64{report.Load.Load1, report.Load.Load5, report.Load.Load15},
		},
		Metadata: &reportv1.AgentMetadata{},
	}
	if includePrivate {
		result.Metadata.Version = client.Version
	}
	if report.Ram.Total > 0 {
		result.Resources.MemoryPercent = float64(report.Ram.Used) * 100 / float64(report.Ram.Total)
	}
	result.NetworkInterfaces = []*reportv1.NetworkInterface{{
		Name: "aggregate", BytesSent: uint64(max(report.Network.TotalUp, 0)), BytesReceived: uint64(max(report.Network.TotalDown, 0)),
	}}
	result.Disks = []*reportv1.DiskInfo{{
		MountPoint: "aggregate", TotalBytes: uint64(max(report.Disk.Total, 0)), UsedBytes: uint64(max(report.Disk.Used, 0)),
	}}
	if report.Disk.Total > 0 {
		result.Disks[0].UsagePercent = float64(report.Disk.Used) * 100 / float64(report.Disk.Total)
	}
	return result
}

func browserStatus(online bool) browserv1.AgentStatus {
	if online {
		return browserv1.AgentStatus_AGENT_STATUS_ONLINE
	}
	return browserv1.AgentStatus_AGENT_STATUS_OFFLINE
}

func stringSet(values []string) map[string]bool {
	result := make(map[string]bool, len(values))
	for _, value := range values {
		result[value] = true
	}
	return result
}

func stringValue(values map[string]interface{}, key string) string {
	value, _ := values[key].(string)
	return value
}

func boolValue(values map[string]interface{}, key string) bool {
	value, _ := values[key].(bool)
	return value
}

func intValue(values map[string]interface{}, key string) int {
	switch value := values[key].(type) {
	case int:
		return value
	case int64:
		return int(value)
	case float64:
		return int(value)
	default:
		return 0
	}
}

func structValue(values map[string]interface{}, key string) (*structpb.Struct, error) {
	switch value := values[key].(type) {
	case map[string]interface{}:
		return structpb.NewStruct(value)
	case gin.H:
		return structpb.NewStruct(map[string]interface{}(value))
	default:
		return structpb.NewStruct(map[string]interface{}{})
	}
}
