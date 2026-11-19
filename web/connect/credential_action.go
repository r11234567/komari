package connectapi

import (
	"context"
	"net/http"

	"github.com/komari-monitor/komari/internal/credentialaction"
	"github.com/komari-monitor/komari/internal/credentialgateway"
	credential "github.com/komari-monitor/komari/internal/trustedcredential"
	"github.com/komari-monitor/komari/pkg/rpc"
	commonv1 "github.com/r11234567/komari-proto/gen/go/komari/common/v1"
)

// Consumers choose the action and authoritative target from their parsed RPC,
// never from a caller-supplied resource string. Retain direct TOTP compatibility.
func verifyConnectCredential(ctx context.Context, proof *commonv1.TwoFactorProof, action string, targets ...string) error {
	if token := proof.GetAuthorizationToken(); token != "" {
		_, err := credentialgateway.Default.Consume(ctx, token, credential.SensitiveOperation, credentialaction.ScopedResource(action, targets...))
		if err != nil {
			return credentialConnectError(err)
		}
		return nil
	}
	return verifyConnectTwoFactor(rpc.MetaFromContext(ctx), proof)
}

// verifyConnectHeaderCredential validates step-up credentials passed via HTTP
// headers for Connect RPCs whose protobuf request schema does not carry an
// inline TwoFactorProof field.
func verifyConnectHeaderCredential(ctx context.Context, header http.Header, action string, targets ...string) error {
	code := header.Get("X-2FA-Code")
	if code == "" {
		code = header.Get("X-Two-Factor-Code")
	}
	proof := &commonv1.TwoFactorProof{
		AuthorizationToken: header.Get(credentialaction.Header),
		Code:               code,
	}
	return verifyConnectCredential(ctx, proof, action, targets...)
}
