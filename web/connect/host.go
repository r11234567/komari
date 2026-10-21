package connectapi

import (
	"context"
	"errors"
	"time"

	"connectrpc.com/connect"
	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/enrollment"
	"github.com/komari-monitor/komari/database/privilegeddelivery"
	"github.com/komari-monitor/komari/pkg/rpc"
	hostv1 "github.com/r11234567/komari-proto/gen/go/komari/host/v1"
	hostv1connect "github.com/r11234567/komari-proto/gen/go/komari/host/v1/hostv1connect"
	securityv1 "github.com/r11234567/komari-proto/gen/go/komari/security/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type hostService struct {
	hostv1connect.UnimplementedHostServiceHandler
}

func (s *hostService) NegotiateHostProtocol(ctx context.Context, req *connect.Request[hostv1.NegotiateHostProtocolRequest]) (*connect.Response[hostv1.NegotiateHostProtocolResponse], error) {
	id, err := requireAgent(rpc.MetaFromContext(ctx), req.Msg.AgentId)
	if err != nil {
		return nil, err
	}
	if req.Msg.ProtocolVersion != 2 {
		return nil, connectError(connect.CodeFailedPrecondition, errors.New("only privileged_delivery_v2 can be negotiated"))
	}
	actions := make([]int32, 0, len(req.Msg.SupportedActions))
	for _, a := range req.Msg.SupportedActions {
		actions = append(actions, int32(a))
	}
	state, err := privilegeddelivery.Negotiate(dbcore.GetDBInstance(), id, req.Msg.ServiceId, req.Msg.StartupId, int32(req.Msg.PrivilegeMode), actions)
	if err != nil {
		return nil, connectError(connect.CodeFailedPrecondition, err)
	}
	return connect.NewResponse(&hostv1.NegotiateHostProtocolResponse{ProtocolVersion: 2, UpdateSequence: state.UpdateSequence}), nil
}
func (s *hostService) PrepareHostOperation(ctx context.Context, req *connect.Request[hostv1.PrepareHostOperationRequest]) (*connect.Response[hostv1.PrepareHostOperationResponse], error) {
	id, err := requireAgent(rpc.MetaFromContext(ctx), req.Msg.AgentId)
	if err != nil {
		return nil, err
	}
	req.Msg.AgentId = id
	op, err := privilegeddelivery.PrepareOperation(dbcore.GetDBInstance(), req.Msg)
	if err != nil {
		return nil, connectError(connect.CodeFailedPrecondition, err)
	}
	if err := signHostOperation(op); err != nil {
		return nil, connectError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&hostv1.PrepareHostOperationResponse{Operation: op}), nil
}
func (s *hostService) ReportHostOperation(ctx context.Context, req *connect.Request[hostv1.ReportHostOperationRequest]) (*connect.Response[hostv1.ReportHostOperationResponse], error) {
	id, err := requireAgent(rpc.MetaFromContext(ctx), req.Msg.AgentId)
	if err != nil {
		return nil, err
	}
	req.Msg.AgentId = id
	op, err := privilegeddelivery.ReportOperation(dbcore.GetDBInstance(), req.Msg)
	if err != nil {
		return nil, connectError(connect.CodeFailedPrecondition, err)
	}
	return connect.NewResponse(&hostv1.ReportHostOperationResponse{Operation: op, Accepted: true}), nil
}
func (s *hostService) GetHostOperation(ctx context.Context, req *connect.Request[hostv1.GetHostOperationRequest]) (*connect.Response[hostv1.GetHostOperationResponse], error) {
	id, err := requireAgent(rpc.MetaFromContext(ctx), req.Msg.AgentId)
	if err != nil {
		return nil, err
	}
	req.Msg.AgentId = id
	op, err := privilegeddelivery.GetOperation(dbcore.GetDBInstance(), req.Msg.AgentId, req.Msg.OperationId)
	if err != nil {
		return nil, connectError(connect.CodeNotFound, err)
	}
	if err := signHostOperation(op); err != nil {
		return nil, connectError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&hostv1.GetHostOperationResponse{Operation: op}), nil
}

// This signature binds the entire lease, including operation ID and expiry.
// Signing HostOperation also binds its embedded HostAuthorization. A recovered
// record retains its original lease; signing never extends the authorization.
func signHostOperation(op *hostv1.HostOperation) error {
	copy := proto.Clone(op).(*hostv1.HostOperation)
	copy.Signature = nil
	body, err := proto.MarshalOptions{Deterministic: true}.Marshal(copy)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	expiry := op.LeaseExpiresAt.AsTime()
	payload, err := proto.MarshalOptions{Deterministic: true}.Marshal(&securityv1.SignedInstruction{AgentId: op.Authorization.AgentId, Nonce: op.OperationId, IssuedAt: timestamppb.New(now), ExpiresAt: timestamppb.New(expiry), InstructionType: "komari.host.v1.HostOperation", Body: body})
	if err != nil {
		return err
	}
	key, algorithm, value, err := enrollment.Sign(payload)
	if err != nil {
		return err
	}
	op.Signature = &securityv1.SignedEnvelope{Payload: payload, Signatures: []*securityv1.Signature{{KeyId: key, Algorithm: securityv1.SignatureAlgorithm(algorithm), Value: value}}}
	return nil
}
