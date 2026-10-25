package connectapi

// MaintenanceService is the production transport for the administration
// console's housekeeping surfaces. The matching admin:* RPC2 methods stay
// registered for unconverted themes but no longer have REST bridge routes.

import (
	"context"
	"errors"
	"strings"

	"connectrpc.com/connect"
	"github.com/komari-monitor/komari/database/accounts"
	"github.com/komari-monitor/komari/database/auditlog"
	"github.com/komari-monitor/komari/database/records"
	"github.com/komari-monitor/komari/database/tasks"
	"github.com/komari-monitor/komari/pkg/rpc"
	jsonRpc "github.com/komari-monitor/komari/web/rpc/jsonrpc"
	adminv1 "github.com/r11234567/komari-proto/gen/go/komari/admin/v1"
	adminv1connect "github.com/r11234567/komari-proto/gen/go/komari/admin/v1/adminv1connect"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	defaultAuditLogLimit = 100
	maxAuditLogLimit     = 1000
)

type maintenanceService struct {
	adminv1connect.UnimplementedMaintenanceServiceHandler
}

func (s *maintenanceService) ListSessions(ctx context.Context, _ *connect.Request[adminv1.ListSessionsRequest]) (*connect.Response[adminv1.ListSessionsResponse], error) {
	stored, err := accounts.GetAllSessions()
	if err != nil {
		return nil, connectError(connect.CodeInternal, err)
	}
	sessions := make([]*adminv1.AdminSession, 0, len(stored))
	for _, session := range stored {
		sessions = append(sessions, &adminv1.AdminSession{
			Uuid:            session.UUID,
			Session:         session.Session,
			UserAgent:       session.UserAgent,
			Ip:              session.Ip,
			LoginMethod:     session.LoginMethod,
			LatestOnline:    timestamppb.New(session.LatestOnline),
			LatestUserAgent: session.LatestUserAgent,
			LatestIp:        session.LatestIp,
			Expires:         timestamppb.New(session.Expires),
			CreatedAt:       timestamppb.New(session.CreatedAt),
		})
	}
	current := ""
	if meta := rpc.MetaFromContext(ctx); meta != nil {
		// Sessions are stored as digests, so the caller's raw cookie would never
		// match a row and the client could not tell which entry is its own. Report
		// the same form the list carries.
		current = accounts.StoredSessionKey(meta.SessionToken)
	}
	return connect.NewResponse(&adminv1.ListSessionsResponse{CurrentSession: current, Sessions: sessions}), nil
}

func (s *maintenanceService) DeleteSession(ctx context.Context, req *connect.Request[adminv1.DeleteSessionRequest]) (*connect.Response[adminv1.DeleteSessionResponse], error) {
	session := strings.TrimSpace(req.Msg.Session)
	if session == "" {
		return nil, connectError(connect.CodeInvalidArgument, errors.New("session is required"))
	}
	if err := accounts.DeleteSession(session); err != nil {
		return nil, connectError(connect.CodeInternal, err)
	}
	auditMaintenance(ctx, "delete session", "info")
	return connect.NewResponse(&adminv1.DeleteSessionResponse{}), nil
}

func (s *maintenanceService) DeleteAllSessions(ctx context.Context, _ *connect.Request[adminv1.DeleteAllSessionsRequest]) (*connect.Response[adminv1.DeleteAllSessionsResponse], error) {
	if err := accounts.DeleteAllSessions(); err != nil {
		return nil, connectError(connect.CodeInternal, err)
	}
	auditMaintenance(ctx, "delete all sessions", "warn")
	return connect.NewResponse(&adminv1.DeleteAllSessionsResponse{}), nil
}

func (s *maintenanceService) ListAuditLogs(_ context.Context, req *connect.Request[adminv1.ListAuditLogsRequest]) (*connect.Response[adminv1.ListAuditLogsResponse], error) {
	limit := int(req.Msg.Limit)
	if limit <= 0 {
		limit = defaultAuditLogLimit
	}
	if limit > maxAuditLogLimit {
		return nil, connectError(connect.CodeInvalidArgument, errors.New("limit is above the supported maximum"))
	}
	page := int(req.Msg.Page)
	if page <= 0 {
		page = 1
	}
	stored, total, err := jsonRpc.AuditLogPage(limit, page, req.Msg.MessageType)
	if err != nil {
		return nil, connectError(connect.CodeInternal, err)
	}
	logs := make([]*adminv1.AuditLogEntry, 0, len(stored))
	for _, entry := range stored {
		logs = append(logs, &adminv1.AuditLogEntry{
			Id:          uint64(entry.ID),
			Ip:          entry.IP,
			Uuid:        entry.UUID,
			Message:     entry.Message,
			MessageType: entry.MsgType,
			Time:        timestamppb.New(entry.Time),
		})
	}
	return connect.NewResponse(&adminv1.ListAuditLogsResponse{Logs: logs, Total: total}), nil
}

func (s *maintenanceService) ClearRecords(ctx context.Context, req *connect.Request[adminv1.ClearRecordsRequest]) (*connect.Response[adminv1.ClearRecordsResponse], error) {
	if err := records.DeleteAll(); err != nil {
		return nil, connectError(connect.CodeInternal, err)
	}
	if !req.Msg.IncludePingRecords {
		auditMaintenance(ctx, "clear records", "warn")
		return connect.NewResponse(&adminv1.ClearRecordsResponse{}), nil
	}
	if err := tasks.DeleteAllPingRecords(); err != nil {
		return nil, connectError(connect.CodeInternal, err)
	}
	auditMaintenance(ctx, "clear all records", "info")
	return connect.NewResponse(&adminv1.ClearRecordsResponse{}), nil
}

func (s *maintenanceService) GetDatabaseStatus(ctx context.Context, _ *connect.Request[adminv1.GetDatabaseStatusRequest]) (*connect.Response[adminv1.GetDatabaseStatusResponse], error) {
	return connect.NewResponse(&adminv1.GetDatabaseStatusResponse{Status: databaseToProto(jsonRpc.DatabaseStatus(ctx))}), nil
}

func (s *maintenanceService) VacuumDatabase(ctx context.Context, _ *connect.Request[adminv1.VacuumDatabaseRequest]) (*connect.Response[adminv1.VacuumDatabaseResponse], error) {
	report, err := jsonRpc.VacuumDatabases(ctx)
	if errors.Is(err, jsonRpc.ErrDatabaseMaintenanceBusy) {
		return nil, connectError(connect.CodeAborted, err)
	}
	if err != nil {
		return nil, connectError(connect.CodeInternal, err)
	}
	message, level := "reclaimed database space", "warn"
	if !report.AllSucceeded {
		message, level = "database space reclaim completed with errors", "error"
	}
	auditMaintenance(ctx, message, level)
	return connect.NewResponse(&adminv1.VacuumDatabaseResponse{Result: &adminv1.DatabaseMaintenance{
		BeforeBytes:  report.Before,
		AfterBytes:   report.After,
		SizeBytes:    report.Size,
		AllSucceeded: report.AllSucceeded,
		Main:         maintenanceResultToProto(report.Main),
		Monitoring:   maintenanceResultToProto(report.Monitoring),
	}}), nil
}

func maintenanceResultToProto(result jsonRpc.DatabaseMaintenanceOutcome) *adminv1.DatabaseMaintenanceResult {
	return &adminv1.DatabaseMaintenanceResult{
		Driver:      result.Driver,
		Action:      result.Action,
		BeforeBytes: result.Before,
		AfterBytes:  result.After,
		Success:     result.Success,
		Error:       result.Error,
		SizeError:   result.SizeError,
	}
}

func auditMaintenance(ctx context.Context, message, level string) {
	meta := rpc.MetaFromContext(ctx)
	if meta == nil {
		auditlog.Log("", "", message, level)
		return
	}
	auditlog.Log(meta.RemoteIP, meta.UserUUID, message, level)
}
