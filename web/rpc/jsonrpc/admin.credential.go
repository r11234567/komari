package jsonrpc

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/komari-monitor/komari/internal/credentialgateway"
	credential "github.com/komari-monitor/komari/internal/trustedcredential"
	"github.com/komari-monitor/komari/pkg/rpc"
)

func init() {
	for name, handler := range map[string]rpc.Handler{
		"listTrustedCredentialProviders":        adminListCredentialProviders,
		"beginTrustedCredentialVerification":    adminBeginCredentialVerification,
		"completeTrustedCredentialVerification": adminCompleteCredentialVerification,
		"cancelTrustedCredentialVerification":   adminCancelCredentialVerification,
	} {
		RegisterWithGroupAndMeta(name, rpc.RoleAdmin, handler, &rpc.MethodMeta{Name: "admin:" + name, Summary: "Interactive trusted credential verification (live session required)"})
	}
}

func adminListCredentialProviders(ctx context.Context, req *rpc.JsonRpcRequest) (any, *rpc.JsonRpcError) {
	var p struct {
		Purpose credential.Purpose `json:"purpose"`
	}
	if err := req.BindParams(&p); err != nil {
		return nil, credentialRPCError(credential.ErrInvalidArgument)
	}
	items, err := credentialgateway.Default.List(ctx, p.Purpose)
	return map[string]any{"providers": items}, credentialRPCError(err)
}
func adminBeginCredentialVerification(ctx context.Context, req *rpc.JsonRpcRequest) (any, *rpc.JsonRpcError) {
	var p credential.Request
	if err := req.BindParams(&p); err != nil {
		return nil, credentialRPCError(credential.ErrInvalidArgument)
	}
	result, err := credentialgateway.Default.Begin(ctx, p)
	return result, credentialRPCError(err)
}
func adminCompleteCredentialVerification(ctx context.Context, req *rpc.JsonRpcRequest) (any, *rpc.JsonRpcError) {
	var p struct {
		ChallengeID string          `json:"challenge_id"`
		Proof       json.RawMessage `json:"proof"`
	}
	if err := req.BindParams(&p); err != nil {
		return nil, credentialRPCError(credential.ErrInvalidArgument)
	}
	result, err := credentialgateway.Default.Complete(ctx, p.ChallengeID, p.Proof)
	return map[string]any{"authorization": result}, credentialRPCError(err)
}
func adminCancelCredentialVerification(ctx context.Context, req *rpc.JsonRpcRequest) (any, *rpc.JsonRpcError) {
	var p struct {
		ChallengeID string `json:"challenge_id"`
	}
	if err := req.BindParams(&p); err != nil {
		return nil, credentialRPCError(credential.ErrInvalidArgument)
	}
	result, err := credentialgateway.Default.Cancel(ctx, p.ChallengeID)
	return map[string]any{"cancelled": result}, credentialRPCError(err)
}
func credentialRPCError(err error) *rpc.JsonRpcError {
	if err == nil {
		return nil
	}
	code := rpc.InternalError
	switch {
	case errors.Is(err, context.Canceled):
		code = rpc.Cancelled
	case errors.Is(err, context.DeadlineExceeded):
		code = rpc.DeadlineExceeded
	case errors.Is(err, credential.ErrInvalidArgument):
		code = rpc.InvalidParams
	case errors.Is(err, credential.ErrUnauthenticated), errors.Is(err, credential.ErrVerificationFailed):
		code = rpc.Unauthenticated
	case errors.Is(err, credential.ErrProviderUnavailable):
		code = rpc.Unavailable
	case errors.Is(err, credential.ErrNotFound):
		code = rpc.NotFound
	case errors.Is(err, credential.ErrBusy):
		code = rpc.Aborted
	case errors.Is(err, credential.ErrLimit):
		code = rpc.OutOfRange
	default:
		err = errors.New("credential operation failed")
	}
	return rpc.MakeError(code, err.Error(), nil)
}
