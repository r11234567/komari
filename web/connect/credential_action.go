package connectapi

import (
	"context"

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
