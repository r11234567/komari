package connectapi

import (
	"context"
	"errors"
	"time"

	"connectrpc.com/connect"
	"github.com/komari-monitor/komari/pkg/rpc"
	legacyv1 "github.com/komari-monitor/komari/protocol/v1"
	clientapi "github.com/komari-monitor/komari/web/api/client"
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
	report := protoReportToLegacy(req.Msg.Report)
	if err := clientapi.IngestReportContext(ctx, agentID, report, 3, true); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, connectError(connect.CodeInvalidArgument, err)
	}
	return connect.NewResponse(&reportv1.SubmitReportResponse{
		Accepted:           true,
		AcceptedSequence:   req.Msg.Report.Sequence,
		NextReportInterval: durationpb.New(3 * time.Second),
		ServerTime:         timestamppb.Now(),
	}), nil
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
		result.Ram.Total = int64FromUint64(report.Resources.MemoryUsedBytes + report.Resources.MemoryAvailableBytes)
		result.Swap.Used = int64FromUint64(report.Resources.SwapUsedBytes)
		result.Swap.Total = int64FromUint64(report.Resources.SwapTotalBytes)
		if len(report.Resources.LoadAverage) > 0 {
			result.Load.Load1 = report.Resources.LoadAverage[0]
		}
		if len(report.Resources.LoadAverage) > 1 {
			result.Load.Load5 = report.Resources.LoadAverage[1]
		}
		if len(report.Resources.LoadAverage) > 2 {
			result.Load.Load15 = report.Resources.LoadAverage[2]
		}
	}
	result.Message = report.DiagnosticMessage
	for _, network := range report.NetworkInterfaces {
		result.Network.TotalUp += int64FromUint64(network.BytesSent)
		result.Network.TotalDown += int64FromUint64(network.BytesReceived)
	}
	for _, disk := range report.Disks {
		result.Disk.Total += int64FromUint64(disk.TotalBytes)
		result.Disk.Used += int64FromUint64(disk.UsedBytes)
	}
	return result
}
