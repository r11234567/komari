package client

import (
	"context"
	"time"

	"github.com/komari-monitor/komari/database/clients"
	"github.com/komari-monitor/komari/database/metricstore"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/database/tasks"
	v1 "github.com/komari-monitor/komari/protocol/v1"
	agent_runtime "github.com/komari-monitor/komari/web/agent"
)

// ingest.go
// agent 上报数据的传输无关处理逻辑。v1 (REST/WS) 与 v2 (JSON-RPC) 两套上报入口
// 经过各自的协议解析后，统一调用这里的函数落库并更新运行时状态，消除重复。

// ingestReport 保存一次负载上报并刷新运行时状态。
// protocolVersion 标记上报所用协议（1 或 2），用于运行时区分客户端能力。
// markPresence 为 true 时按 POST 上报会话刷新在线状态（WS 连接自行管理在线状态，应传 false）。
func ingestReport(uuid string, report v1.Report, protocolVersion int, markPresence bool) error {
	return ingestReportContext(context.Background(), uuid, report, protocolVersion, markPresence)
}

func ingestReportContext(ctx context.Context, uuid string, report v1.Report, protocolVersion int, markPresence bool) error {
	report.UUID = uuid
	report.UpdatedAt = time.Now().UTC()
	if err := clients.ReportVerify(report); err != nil {
		return err
	}
	savedReport, err := metricstore.WriteReport(ctx, report)
	if err != nil {
		return err
	}
	agent_runtime.RecordReport(savedReport)
	agent_runtime.SetClientProtocolVersion(uuid, protocolVersion)
	if markPresence {
		refreshPostPresence(uuid)
	}
	return nil
}

// IngestReport is the transport-neutral entry point used by Connect handlers.
// Legacy v1/v2 routes continue to call the private helper above.
func IngestReport(uuid string, report v1.Report, protocolVersion int, markPresence bool) error {
	return ingestReport(uuid, report, protocolVersion, markPresence)
}

// IngestReportContext propagates Connect cancellation and deadlines to storage.
func IngestReportContext(ctx context.Context, uuid string, report v1.Report, protocolVersion int, markPresence bool) error {
	return ingestReportContext(ctx, uuid, report, protocolVersion, markPresence)
}

// IngestBasicInfo is the transport-neutral entry point for typed system
// identity reports. The caller owns protocol decoding and authentication.
func IngestBasicInfo(uuid string, info map[string]interface{}, fallbackIP string) error {
	return ingestBasicInfo(uuid, info, fallbackIP)
}

// TouchConnectPresence refreshes online state for a typed non-WebSocket Agent
// without writing another metric sample. Connect report and metrics services
// call it because both requests prove that the authenticated Agent is alive.
func TouchConnectPresence(uuid string) {
	refreshPostPresence(uuid)
	agent_runtime.SetClientProtocolVersion(uuid, 3)
}

// ingestBasicInfo 保存客户端基础信息。fallbackIP 在上报未携带 IP 时用作兜底。
func ingestBasicInfo(uuid string, info map[string]interface{}, fallbackIP string) error {
	if info == nil {
		info = map[string]interface{}{}
	}
	return saveClientBasicInfo(info, uuid, fallbackIP)
}

// ingestPingResult 保存一条 ping 探测结果。
func ingestPingResult(uuid string, taskID uint, value int) error {
	return tasks.SavePingRecord(models.PingRecord{
		Client: uuid,
		TaskId: taskID,
		Value:  value,
		Time:   time.Now().UTC(),
	})
}
