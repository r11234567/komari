package connectapi

// Machine enrollment approval.
//
// The approval step is where an administrator decides what a pending request
// becomes. Two paths exist:
//
//   - Create a new machine: grants nothing that did not exist before, so it
//     costs only a session, not a second factor.
//   - Bind to an existing machine: hands the requester everything belonging to
//     an existing machine — its history, delivered configuration and access —
//     so it requires a fresh second factor.
//
// Neither path is reachable without a session, which is the one gate that made
// the BeginEnrollment/PollEnrollment cycle safe to expose without credentials.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"connectrpc.com/connect"
	"github.com/komari-monitor/komari/database/auditlog"
	"github.com/komari-monitor/komari/database/clients"
	"github.com/komari-monitor/komari/database/enrollment"
	"github.com/komari-monitor/komari/pkg/rpc"
	enrollmentv1 "github.com/r11234567/komari-proto/gen/go/komari/enrollment/v1"
	"github.com/r11234567/komari-proto/gen/go/komari/enrollment/v1/enrollmentv1connect"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type enrollmentAdminService struct {
	enrollmentv1connect.UnimplementedEnrollmentAdminServiceHandler
}

func (s *enrollmentAdminService) ListPendingEnrollments(ctx context.Context, req *connect.Request[enrollmentv1.ListPendingEnrollmentsRequest]) (*connect.Response[enrollmentv1.ListPendingEnrollmentsResponse], error) {
	meta := rpc.MetaFromContext(ctx)
	if meta == nil || meta.Principal == nil || !meta.Principal.HasRole(rpc.RoleAdmin) {
		return nil, connectError(connect.CodePermissionDenied, errors.New("administrator access is required"))
	}
	attempts, err := enrollment.ListPending()
	if err != nil {
		return nil, connectError(connect.CodeInternal, err)
	}
	result := make([]*enrollmentv1.PendingEnrollment, 0, len(attempts))
	for _, attempt := range attempts {
		result = append(result, pendingEnrollmentToProto(attempt))
	}
	return connect.NewResponse(&enrollmentv1.ListPendingEnrollmentsResponse{Enrollments: result}), nil
}

func (s *enrollmentAdminService) GetPendingEnrollment(ctx context.Context, req *connect.Request[enrollmentv1.GetPendingEnrollmentRequest]) (*connect.Response[enrollmentv1.GetPendingEnrollmentResponse], error) {
	meta := rpc.MetaFromContext(ctx)
	if meta == nil || meta.Principal == nil || !meta.Principal.HasRole(rpc.RoleAdmin) {
		return nil, connectError(connect.CodePermissionDenied, errors.New("administrator access is required"))
	}
	if strings.TrimSpace(req.Msg.GetUserCode()) == "" {
		return nil, connectError(connect.CodeInvalidArgument, errors.New("a verification code is required"))
	}
	attempt, err := enrollment.GetByUserCode(req.Msg.GetUserCode())
	if err != nil {
		return nil, connectError(connect.CodeNotFound, errors.New("verification code not found or expired"))
	}
	candidates, err := enrollment.Candidates(attempt)
	if err != nil {
		return nil, connectError(connect.CodeInternal, err)
	}
	proto := pendingEnrollmentToProto(attempt)
	protoCandiates := make([]*enrollmentv1.EnrollmentCandidate, 0, len(candidates))
	for _, c := range candidates {
		protoCandiates = append(protoCandiates, &enrollmentv1.EnrollmentCandidate{
			AgentId: c.ClientUUID, Name: c.Name,
			MatchReason: c.MatchReason, FingerprintMatch: c.FingerprintMatch,
		})
	}
	return connect.NewResponse(&enrollmentv1.GetPendingEnrollmentResponse{
		Enrollment: proto, Candidates: protoCandiates,
	}), nil
}

func (s *enrollmentAdminService) ApproveEnrollment(ctx context.Context, req *connect.Request[enrollmentv1.ApproveEnrollmentRequest]) (*connect.Response[enrollmentv1.ApproveEnrollmentResponse], error) {
	meta := rpc.MetaFromContext(ctx)
	if meta == nil || meta.Principal == nil || !meta.Principal.HasRole(rpc.RoleAdmin) {
		return nil, connectError(connect.CodePermissionDenied, errors.New("administrator access is required"))
	}
	if strings.TrimSpace(req.Msg.GetUserCode()) == "" {
		return nil, connectError(connect.CodeInvalidArgument, errors.New("a verification code is required"))
	}

	var clientUUID string
	created := false

	switch target := req.Msg.Target.(type) {
	case *enrollmentv1.ApproveEnrollmentRequest_ExistingAgentId:
		// Binding to an existing machine hands over everything it owns, so a
		// fresh second factor is required even if the administrator already has
		// a session.
		if err := verifyConnectTwoFactor(meta, req.Msg.GetTwoFactor()); err != nil {
			return nil, connectError(connect.CodeUnauthenticated, err)
		}
		uuid := strings.TrimSpace(target.ExistingAgentId)
		if uuid == "" {
			return nil, connectError(connect.CodeInvalidArgument, errors.New("an existing agent ID is required"))
		}
		if _, err := clients.GetClientByUUID(uuid); err != nil {
			return nil, connectError(connect.CodeNotFound, fmt.Errorf("agent %s was not found", uuid))
		}
		clientUUID = uuid

	case *enrollmentv1.ApproveEnrollmentRequest_NewAgent:
		name := strings.TrimSpace(target.NewAgent.GetName())
		newUUID, _, err := clients.CreateClientWithName(name)
		if err != nil {
			return nil, connectError(connect.CodeInternal, err)
		}
		clientUUID = newUUID
		created = true
		updates := map[string]interface{}{"uuid": newUUID}
		if v := strings.TrimSpace(target.NewAgent.GetGroup()); v != "" {
			updates["group"] = v
		}
		if v := strings.TrimSpace(target.NewAgent.GetRemark()); v != "" {
			updates["remark"] = v
		}
		if v := strings.TrimSpace(target.NewAgent.GetPublicRemark()); v != "" {
			updates["public_remark"] = v
		}
		if target.NewAgent.GetHidden() {
			updates["hidden"] = true
		}
		if tags := target.NewAgent.GetTags(); len(tags) > 0 {
			updates["tags"] = strings.Join(tags, ";")
		}
		if len(updates) > 1 {
			_ = clients.SaveClient(updates)
		}

	default:
		return nil, connectError(connect.CodeInvalidArgument, errors.New("either an existing agent ID or new agent details are required"))
	}

	approvedAttempt, err := enrollment.Approve(req.Msg.GetUserCode(), clientUUID, meta.Principal.UserUUID)
	if err != nil {
		if created {
			// A just-created machine that cannot be bound to the request is
			// cleaned up rather than left as a ghost in the panel.
			_ = clients.DeleteClient(clientUUID)
		}
		return nil, connectError(connect.CodeFailedPrecondition, err)
	}

	action := "bound to"
	if created {
		action = "created as"
	}
	auditlog.Log(meta.RemoteIP, meta.Principal.UserUUID,
		fmt.Sprintf("enrollment approved: code=%s %s agent=%s fingerprint=%s",
			req.Msg.GetUserCode(), action, clientUUID, approvedAttempt.HostFingerprint), "warn")

	return connect.NewResponse(&enrollmentv1.ApproveEnrollmentResponse{
		AgentId: clientUUID, Enrollment: pendingEnrollmentToProto(approvedAttempt), Created: created,
	}), nil
}

func (s *enrollmentAdminService) DenyEnrollment(ctx context.Context, req *connect.Request[enrollmentv1.DenyEnrollmentRequest]) (*connect.Response[enrollmentv1.DenyEnrollmentResponse], error) {
	meta := rpc.MetaFromContext(ctx)
	if meta == nil || meta.Principal == nil || !meta.Principal.HasRole(rpc.RoleAdmin) {
		return nil, connectError(connect.CodePermissionDenied, errors.New("administrator access is required"))
	}
	if err := enrollment.Deny(req.Msg.GetUserCode(), meta.Principal.UserUUID); err != nil {
		return nil, connectError(connect.CodeFailedPrecondition, err)
	}
	auditlog.Log(meta.RemoteIP, meta.Principal.UserUUID,
		"enrollment denied: code="+req.Msg.GetUserCode(), "warn")
	return connect.NewResponse(&enrollmentv1.DenyEnrollmentResponse{}), nil
}

func pendingEnrollmentToProto(attempt enrollment.Attempt) *enrollmentv1.PendingEnrollment {
	e := &enrollmentv1.PendingEnrollment{
		UserCode:            attempt.UserCode,
		State:               enrollmentStateToProto(attempt.State),
		AgentKeyFingerprint: attempt.KeyFingerprint(),
		RemoteIp:            attempt.RemoteIP,
		AgentId:             attempt.Client,
		CreatedAt:           timestamppb.New(attempt.CreatedAt),
		ExpiresAt:           timestamppb.New(attempt.ExpiresAt),
	}
	if attempt.Hostname != "" || attempt.OperatingSystem != "" {
		e.Device = &enrollmentv1.DeviceIdentity{
			Hostname:        attempt.Hostname,
			OperatingSystem: attempt.OperatingSystem,
			Architecture:    attempt.Architecture,
			AgentVersion:    attempt.AgentVersion,
			Fingerprint:     attempt.HostFingerprint,
		}
	}
	return e
}
