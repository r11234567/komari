package connectapi

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"connectrpc.com/connect"
	"github.com/komari-monitor/komari/pkg/requestmeta"
	enrollment "github.com/r11234567/komari-proto/gen/go/komari/enrollment/v1"
	enrollmentconnect "github.com/r11234567/komari-proto/gen/go/komari/enrollment/v1/enrollmentv1connect"
	securityv1 "github.com/r11234567/komari-proto/gen/go/komari/security/v1"
	"github.com/stretchr/testify/require"
)

func TestEntryBoundaryEnrollmentOriginCannotBeChosenByCaller(t *testing.T) {
	entryPrivateSite(t, true)
	server := entryBoundaryServer(t, "https://panel.test")
	t.Setenv(requestmeta.TrustedProxyEnv, "none")
	client := enrollmentconnect.NewEnrollmentServiceClient(server.Client(), server.URL)
	key, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	request := connect.NewRequest(&enrollment.BeginEnrollmentRequest{AgentPublicKey: &securityv1.PublicKey{
		Algorithm: securityv1.SignatureAlgorithm_SIGNATURE_ALGORITHM_ED25519, Value: key,
	}})
	request.Header().Set("Forwarded", "host=evil.test;proto=http")
	request.Header().Set("X-Forwarded-Host", "evil.test")
	request.Header().Set("X-Forwarded-Proto", "http")
	response, err := client.BeginEnrollment(context.Background(), request)
	require.NoError(t, err)
	require.Equal(t, "https://panel.test/bind", response.Msg.VerificationUri)
	require.Equal(t, "https://panel.test/bind?code="+response.Msg.UserCode, response.Msg.VerificationUriComplete)

	t.Setenv(requestmeta.PublicOriginEnv, "")
	response, err = client.BeginEnrollment(context.Background(), request)
	require.NoError(t, err)
	require.Equal(t, "https://panel.test/bind", response.Msg.VerificationUri, "listener policy must not reread process environment")

	direct := entryBoundaryServer(t)
	response, err = enrollmentconnect.NewEnrollmentServiceClient(direct.Client(), direct.URL).BeginEnrollment(context.Background(), request)
	require.NoError(t, err)
	require.Equal(t, direct.URL+"/bind", response.Msg.VerificationUri, "direct deployment must not need an extra environment setting")
}
