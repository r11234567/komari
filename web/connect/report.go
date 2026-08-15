package connectapi

import (
	"context"
	"errors"
	"net"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/komari-monitor/komari/database/clients"
	"github.com/komari-monitor/komari/pkg/rpc"
	legacyv1 "github.com/komari-monitor/komari/protocol/v1"
	agentRuntime "github.com/komari-monitor/komari/web/agent"
	clientapi "github.com/komari-monitor/komari/web/api/client"
	"github.com/komari-monitor/komari/web/rescueapp"
	reportv1 "github.com/r11234567/komari-proto/gen/go/komari/report/v1"
	reportv1connect "github.com/r11234567/komari-proto/gen/go/komari/report/v1/reportv1connect"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type reportService struct {
	reportv1connect.UnimplementedAgentReportServiceHandler
}

func (s *reportService) SubmitReport(ctx context.Context, req *connect.Request[reportv1.SubmitReportRequest]) (*connect.Response[reportv1.SubmitReportResponse], error) {
	if req.Msg.Report == nil {
		return nil, connectError(connect.CodeInvalidArgument, errors.New("report is required"))
	}
	agentID, err := requireAgent(rpc.MetaFromContext(ctx), req.Msg.Report.AgentId)
	if err != nil {
		return nil, err
	}
	if info := protoReportBasicInfo(req.Msg.Report); len(info) > 0 {
		remoteIP := ""
		if meta := rpc.MetaFromContext(ctx); meta != nil {
			remoteIP = meta.RemoteIP
		}
		if err := clientapi.IngestBasicInfo(agentID, info, remoteIP); err != nil {
			return nil, connectError(connect.CodeInternal, err)
		}
	}
	// Resource history is written only by MetricsService. This report owns
	// stable identity, capabilities and applied configuration state, so writing
	// its resource snapshot as well would create duplicate metric points.
	clientapi.TouchConnectPresence(agentID)
	if metadata := req.Msg.Report.Metadata; metadata != nil {
		capabilities := metadata.Capabilities
		agentRuntime.SetConnectCapabilities(agentID, capabilities)
		if metadata.AppliedConfigRevision > 0 {
			if _, err := clients.RecordAppliedDeploymentRevision(agentID, metadata.AppliedConfigRevision); err != nil {
				return nil, connectError(connect.CodeInternal, err)
			}
		}
		if metadata.RescueHelper != nil {
			if err := rescueapp.ReportStatus(agentID, metadata.RescueHelper); err != nil {
				return nil, connectError(connect.CodeInvalidArgument, err)
			}
		}
	} else {
		agentRuntime.SetConnectCapabilities(agentID, nil)
	}
	return connect.NewResponse(&reportv1.SubmitReportResponse{
		Accepted:           true,
		AcceptedSequence:   req.Msg.Report.Sequence,
		NextReportInterval: durationpb.New(3 * time.Second),
		ServerTime:         timestamppb.Now(),
	}), nil
}

func protoReportBasicInfo(report *reportv1.AgentReport) map[string]interface{} {
	result := make(map[string]interface{})
	system := report.System
	if system != nil {
		putString := func(key, value string) {
			if value = strings.TrimSpace(value); value != "" {
				result[key] = value
			}
		}
		putString("cpu_name", system.CpuName)
		putString("arch", system.Architecture)
		putString("os", system.Os)
		putString("kernel_version", system.KernelVersion)
		putString("virtualization", system.Virtualization)
		if system.CpuCount > 0 {
			result["cpu_cores"] = system.CpuCount
		}
		if system.CpuPhysicalCount > 0 {
			result["cpu_physical_cores"] = system.CpuPhysicalCount
		}
		if system.MemoryTotalBytes > 0 {
			result["mem_total"] = system.MemoryTotalBytes
		}
	}
	if resources := report.Resources; resources != nil && resources.SwapTotalBytes > 0 {
		result["swap_total"] = resources.SwapTotalBytes
	}
	var diskTotal uint64
	for _, disk := range report.Disks {
		if disk != nil {
			diskTotal += disk.TotalBytes
		}
	}
	if diskTotal > 0 {
		result["disk_total"] = diskTotal
	}
	gpuNames := make([]string, 0)
	if resources := report.Resources; resources != nil {
		for _, gpu := range resources.Gpus {
			if gpu != nil && strings.TrimSpace(gpu.Name) != "" {
				gpuNames = append(gpuNames, strings.TrimSpace(gpu.Name))
			}
		}
	}
	if len(gpuNames) > 0 {
		result["gpu_name"] = strings.Join(gpuNames, ", ")
	}
	if metadata := report.Metadata; metadata != nil && strings.TrimSpace(metadata.Version) != "" {
		result["version"] = strings.TrimSpace(metadata.Version)
	}
	for _, network := range report.NetworkInterfaces {
		if network == nil {
			continue
		}
		for _, address := range network.Addresses {
			ip := net.ParseIP(strings.TrimSpace(address))
			if ip == nil {
				continue
			}
			if ip.To4() != nil {
				if _, exists := result["ipv4"]; !exists {
					result["ipv4"] = ip.String()
				}
			} else if _, exists := result["ipv6"]; !exists {
				result["ipv6"] = ip.String()
			}
		}
	}
	return result
}

func protoReportToLegacy(report *reportv1.AgentReport) legacyv1.Report {
	result := legacyv1.Report{}
	if report.ObservedAt != nil && report.ObservedAt.IsValid() {
		result.UpdatedAt = report.ObservedAt.AsTime()
	}
	if report.System != nil {
		result.CPU.Cores = int(report.System.CpuCount)
		if report.System.Uptime != nil && report.System.Uptime.IsValid() {
			result.Uptime = int64(report.System.Uptime.AsDuration() / time.Second)
		}
	}
	if report.Resources != nil {
		result.CPU.Usage = report.Resources.CpuPercent
		result.Ram.Used = int64FromUint64(report.Resources.MemoryUsedBytes)
		result.Ram.Total = int64FromUint64(saturatingAdd(report.Resources.MemoryUsedBytes, report.Resources.MemoryAvailableBytes))
		result.Swap.Used = int64FromUint64(report.Resources.SwapUsedBytes)
		result.Swap.Total = int64FromUint64(report.Resources.SwapTotalBytes)
		result.Process = intFromUint64(report.Resources.ProcessCount)
		result.Connections.TCP = intFromUint64(report.Resources.TcpConnectionCount)
		result.Connections.UDP = intFromUint64(report.Resources.UdpConnectionCount)
		if len(report.Resources.LoadAverage) > 0 {
			result.Load.Load1 = report.Resources.LoadAverage[0]
		}
		if len(report.Resources.LoadAverage) > 1 {
			result.Load.Load5 = report.Resources.LoadAverage[1]
		}
		if len(report.Resources.LoadAverage) > 2 {
			result.Load.Load15 = report.Resources.LoadAverage[2]
		}
		if len(report.Resources.Gpus) > 0 {
			result.GPU = &legacyv1.GPUDetailReport{Count: len(report.Resources.Gpus)}
			for _, gpu := range report.Resources.Gpus {
				if gpu == nil {
					continue
				}
				item := legacyv1.GPUDeviceInfo{Name: gpu.Name}
				if gpu.UtilizationPercent != nil {
					item.Utilization = *gpu.UtilizationPercent
					result.GPU.AverageUsage += item.Utilization
				}
				if gpu.MemoryUsedBytes != nil {
					item.MemoryUsed = int64FromUint64(*gpu.MemoryUsedBytes)
				}
				if gpu.MemoryTotalBytes != nil {
					item.MemoryTotal = int64FromUint64(*gpu.MemoryTotalBytes)
				}
				if gpu.TemperatureCelsius != nil {
					item.Temperature = int(*gpu.TemperatureCelsius)
				}
				result.GPU.DetailedInfo = append(result.GPU.DetailedInfo, item)
			}
			if len(result.GPU.DetailedInfo) > 0 {
				result.GPU.AverageUsage /= float64(len(result.GPU.DetailedInfo))
			}
		}
	}
	result.Message = report.DiagnosticMessage
	for _, network := range report.NetworkInterfaces {
		if network == nil {
			continue
		}
		result.Network.TotalUp += int64FromUint64(network.BytesSent)
		result.Network.TotalDown += int64FromUint64(network.BytesReceived)
		result.Network.Up += int64FromUint64(network.BytesSentPerSecond)
		result.Network.Down += int64FromUint64(network.BytesReceivedPerSecond)
	}
	for _, disk := range report.Disks {
		if disk == nil {
			continue
		}
		result.Disk.Total += int64FromUint64(disk.TotalBytes)
		result.Disk.Used += int64FromUint64(disk.UsedBytes)
	}
	return result
}

func saturatingAdd(left, right uint64) uint64 {
	if ^uint64(0)-left < right {
		return ^uint64(0)
	}
	return left + right
}

func intFromUint64(value uint64) int {
	maximum := uint64(^uint(0) >> 1)
	if value > maximum {
		return int(maximum)
	}
	return int(value)
}
