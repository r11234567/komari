package connectapi

import (
	"context"
	"errors"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/komari-monitor/komari/database"
	"github.com/komari-monitor/komari/database/clients"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/pkg/rpc"
	legacyv1 "github.com/komari-monitor/komari/protocol/v1"
	"github.com/komari-monitor/komari/utils"
	agent_runtime "github.com/komari-monitor/komari/web/agent"
	browserv1 "github.com/r11234567/komari-proto/gen/go/komari/browser/v1"
	browserv1connect "github.com/r11234567/komari-proto/gen/go/komari/browser/v1/browserv1connect"
	reportv1 "github.com/r11234567/komari-proto/gen/go/komari/report/v1"
	"google.golang.org/protobuf/types/known/durationpb"
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
	return connect.NewResponse(&browserv1.GetPublicInfoResponse{
		SiteName:        stringValue(info, "sitename"),
		SiteDescription: stringValue(info, "description"),
		Version:         utils.CurrentVersion,
		DefaultTheme:    stringValue(info, "theme"),
	}), nil
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
		result = append(result, browserSummary(item.UUID, item.Name, latest[item.UUID], online[item.UUID]))
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
	if item.Hidden && !isAdmin {
		return nil, connectError(connect.CodeNotFound, errors.New("agent not found"))
	}
	report := agent_runtime.GetLatestReport()[item.UUID]
	online := stringSet(agent_runtime.GetAllOnlineUUIDs())
	return connect.NewResponse(&browserv1.GetAgentResponse{
		Agent:        browserSummary(item.UUID, item.Name, report, online[item.UUID]),
		LatestReport: legacyReportToProto(item, report),
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

func browserSummary(id, name string, report *legacyv1.Report, online bool) *browserv1.AgentSummary {
	result := &browserv1.AgentSummary{AgentId: id, Name: name, Status: browserStatus(online)}
	if report != nil {
		result.LastSeen = timestamppb.New(report.UpdatedAt)
		result.CpuPercent = report.CPU.Usage
		if report.Ram.Total > 0 {
			result.MemoryPercent = float64(report.Ram.Used) * 100 / float64(report.Ram.Total)
		}
	}
	return result
}

func legacyReportToProto(client models.Client, report *legacyv1.Report) *reportv1.AgentReport {
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
		Metadata: &reportv1.AgentMetadata{Version: client.Version},
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
