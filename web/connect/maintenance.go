package connectapi

// MaintenanceService is the production transport for the administration
// console's housekeeping surfaces. The matching admin:* RPC2 methods stay
// registered for unconverted themes but no longer have REST bridge routes.

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"connectrpc.com/connect"
	"github.com/komari-monitor/komari/database/accounts"
	"github.com/komari-monitor/komari/database/auditlog"
	clipboardDB "github.com/komari-monitor/komari/database/clipboard"
	"github.com/komari-monitor/komari/database/models"
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
		current = meta.SessionToken
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

func (s *maintenanceService) ListClipboardEntries(_ context.Context, _ *connect.Request[adminv1.ListClipboardEntriesRequest]) (*connect.Response[adminv1.ListClipboardEntriesResponse], error) {
	stored, err := clipboardDB.ListClipboard()
	if err != nil {
		return nil, connectError(connect.CodeInternal, err)
	}
	entries := make([]*adminv1.ClipboardEntry, 0, len(stored))
	for _, entry := range stored {
		entries = append(entries, clipboardToProto(entry))
	}
	return connect.NewResponse(&adminv1.ListClipboardEntriesResponse{Entries: entries}), nil
}

func (s *maintenanceService) GetClipboardEntry(_ context.Context, req *connect.Request[adminv1.GetClipboardEntryRequest]) (*connect.Response[adminv1.GetClipboardEntryResponse], error) {
	entry, err := clipboardDB.GetClipboardByID(int(req.Msg.Id))
	if err != nil {
		return nil, connectError(connect.CodeNotFound, err)
	}
	if entry == nil {
		return nil, connectError(connect.CodeNotFound, errors.New("clipboard entry not found"))
	}
	return connect.NewResponse(&adminv1.GetClipboardEntryResponse{Entry: clipboardToProto(*entry)}), nil
}

func (s *maintenanceService) CreateClipboardEntry(ctx context.Context, req *connect.Request[adminv1.CreateClipboardEntryRequest]) (*connect.Response[adminv1.CreateClipboardEntryResponse], error) {
	entry := models.Clipboard{
		Name:   req.Msg.Name,
		Text:   req.Msg.Text,
		Weight: int(req.Msg.Weight),
		Remark: req.Msg.Remark,
	}
	if err := clipboardDB.CreateClipboard(&entry); err != nil {
		return nil, connectError(connect.CodeInternal, err)
	}
	auditMaintenance(ctx, "create clipboard:"+strconv.Itoa(entry.Id), "info")
	return connect.NewResponse(&adminv1.CreateClipboardEntryResponse{Entry: clipboardToProto(entry)}), nil
}

func (s *maintenanceService) UpdateClipboardEntry(ctx context.Context, req *connect.Request[adminv1.UpdateClipboardEntryRequest]) (*connect.Response[adminv1.UpdateClipboardEntryResponse], error) {
	fields := map[string]interface{}{}
	if req.Msg.Name != nil {
		fields["name"] = *req.Msg.Name
	}
	if req.Msg.Text != nil {
		fields["text"] = *req.Msg.Text
	}
	if req.Msg.Weight != nil {
		fields["weight"] = int(*req.Msg.Weight)
	}
	if req.Msg.Remark != nil {
		fields["remark"] = *req.Msg.Remark
	}
	if len(fields) == 0 {
		return nil, connectError(connect.CodeInvalidArgument, errors.New("no clipboard fields were supplied"))
	}
	if err := clipboardDB.UpdateClipboardFields(int(req.Msg.Id), fields); err != nil {
		return nil, connectError(connect.CodeInternal, err)
	}
	auditMaintenance(ctx, "update clipboard:"+strconv.Itoa(int(req.Msg.Id)), "info")
	return connect.NewResponse(&adminv1.UpdateClipboardEntryResponse{}), nil
}

func (s *maintenanceService) DeleteClipboardEntries(ctx context.Context, req *connect.Request[adminv1.DeleteClipboardEntriesRequest]) (*connect.Response[adminv1.DeleteClipboardEntriesResponse], error) {
	if len(req.Msg.Ids) == 0 {
		return nil, connectError(connect.CodeInvalidArgument, errors.New("ids cannot be empty"))
	}
	ids := make([]int, 0, len(req.Msg.Ids))
	for _, id := range req.Msg.Ids {
		ids = append(ids, int(id))
	}
	if len(ids) == 1 {
		if err := clipboardDB.DeleteClipboard(ids[0]); err != nil {
			return nil, connectError(connect.CodeInternal, err)
		}
		auditMaintenance(ctx, "delete clipboard:"+strconv.Itoa(ids[0]), "warn")
		return connect.NewResponse(&adminv1.DeleteClipboardEntriesResponse{}), nil
	}
	if err := clipboardDB.DeleteClipboardBatch(ids); err != nil {
		return nil, connectError(connect.CodeInternal, err)
	}
	auditMaintenance(ctx, "batch delete clipboard: "+strconv.Itoa(len(ids))+" items", "warn")
	return connect.NewResponse(&adminv1.DeleteClipboardEntriesResponse{}), nil
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

func clipboardToProto(entry models.Clipboard) *adminv1.ClipboardEntry {
	return &adminv1.ClipboardEntry{
		Id:        int32(entry.Id),
		Name:      entry.Name,
		Text:      entry.Text,
		Weight:    int32(entry.Weight),
		Remark:    entry.Remark,
		CreatedAt: timestamppb.New(entry.CreatedAt),
		UpdatedAt: timestamppb.New(entry.UpdatedAt),
	}
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
