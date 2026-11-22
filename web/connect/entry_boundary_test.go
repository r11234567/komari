package connectapi

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/komari-monitor/komari/database/accounts"
	"github.com/komari-monitor/komari/pkg/config"
	"github.com/komari-monitor/komari/pkg/requestmeta"
	"github.com/komari-monitor/komari/web/api"
	"github.com/komari-monitor/komari/web/security"
	"github.com/pquerna/otp/totp"
	browser "github.com/r11234567/komari-proto/gen/go/komari/browser/v1"
	browserconnect "github.com/r11234567/komari-proto/gen/go/komari/browser/v1/browserv1connect"
	credential "github.com/r11234567/komari-proto/gen/go/komari/credential/v1"
	credentialconnect "github.com/r11234567/komari-proto/gen/go/komari/credential/v1/credentialv1connect"
	enrollment "github.com/r11234567/komari-proto/gen/go/komari/enrollment/v1"
	enrollmentconnect "github.com/r11234567/komari-proto/gen/go/komari/enrollment/v1/enrollmentv1connect"
	securityv1 "github.com/r11234567/komari-proto/gen/go/komari/security/v1"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/structpb"
)

// Exercise the complete entry stack, not just service methods. Previous login
// and refresh tests bypassed the private-site gate and missed this regression.
func entryBoundaryServer(t *testing.T, publicOrigin ...string) *httptest.Server {
	t.Helper()
	pinned := ""
	if len(publicOrigin) != 0 {
		pinned = publicOrigin[0]
	}
	t.Setenv(requestmeta.PublicOriginEnv, pinned)
	t.Setenv(requestmeta.TrustedProxyEnv, "none")
	router := gin.New()
	require.NoError(t, api.ConfigureEntryBoundary(router, security.NewCorsController(true, ""), security.NewRateLimitController(false)))
	Register(router)
	server := httptest.NewUnstartedServer(router)
	server.EnableHTTP2 = true
	server.StartTLS()
	t.Cleanup(server.Close)
	return server
}

func entryPrivateSite(t *testing.T, enabled bool) {
	t.Helper()
	previous, err := config.GetAs[bool](config.PrivateSiteKey, false)
	require.NoError(t, err)
	require.NoError(t, config.Set(config.PrivateSiteKey, enabled))
	t.Cleanup(func() { require.NoError(t, config.Set(config.PrivateSiteKey, previous)) })
}

func TestEntryBoundaryPrivateSitePasswordCeremony(t *testing.T) {
	for _, private := range []bool{false, true} {
		t.Run(map[bool]string{false: "public", true: "private"}[private], func(t *testing.T) {
			entryPrivateSite(t, private)
			server := entryBoundaryServer(t)
			httpClient := server.Client()
			httpClient.Jar, _ = cookiejar.New(nil)
			client := credentialconnect.NewPasswordLoginServiceClient(httpClient, server.URL)
			ctx := context.Background()
			_, err := client.BeginLoginVerification(ctx, connect.NewRequest(&credential.BeginLoginVerificationRequest{}))
			require.Equal(t, connect.CodeUnauthenticated, connect.CodeOf(err))
			_, err = client.CompletePasswordLogin(ctx, connect.NewRequest(&credential.CompletePasswordLoginRequest{Proof: &structpb.Struct{}}))
			require.Equal(t, connect.CodeUnauthenticated, connect.CodeOf(err))

			user, err := accounts.CreateAccount("entry-"+uuid.NewString(), "Entry-boundary-password-123")
			require.NoError(t, err)
			t.Cleanup(func() { _ = accounts.DeleteAccountByUsername(user.Username) })
			key, err := totp.Generate(totp.GenerateOpts{Issuer: "Komari entry fixture", AccountName: user.Username})
			require.NoError(t, err)
			require.NoError(t, accounts.Enable2Fa(user.UUID, key.Secret()))
			started, err := client.BeginPasswordLogin(ctx, connect.NewRequest(&credential.BeginPasswordLoginRequest{Username: user.Username, Password: "Entry-boundary-password-123"}))
			require.NoError(t, err)
			require.False(t, started.Msg.Authenticated)
			cancelled, err := client.CancelPasswordLogin(ctx, connect.NewRequest(&credential.CancelPasswordLoginRequest{}))
			require.NoError(t, err)
			require.True(t, cancelled.Msg.Cancelled)
			_, err = client.BeginLoginVerification(ctx, connect.NewRequest(&credential.BeginLoginVerificationRequest{}))
			require.Equal(t, connect.CodeUnauthenticated, connect.CodeOf(err))

			_, err = client.BeginPasswordLogin(ctx, connect.NewRequest(&credential.BeginPasswordLoginRequest{Username: user.Username, Password: "Entry-boundary-password-123"}))
			require.NoError(t, err)
			challenge, err := client.BeginLoginVerification(ctx, connect.NewRequest(&credential.BeginLoginVerificationRequest{}))
			require.NoError(t, err)
			code, err := totp.GenerateCode(key.Secret(), time.Now())
			require.NoError(t, err)
			proof, err := structpb.NewStruct(map[string]any{"code": code})
			require.NoError(t, err)
			completed, err := client.CompletePasswordLogin(ctx, connect.NewRequest(&credential.CompletePasswordLoginRequest{ChallengeId: challenge.Msg.ChallengeId, Proof: proof}))
			require.NoError(t, err)
			require.True(t, completed.Msg.Authenticated)
			// The native session must work through the same full entry stack.
			viewer, err := browserconnect.NewBrowserServiceClient(httpClient, server.URL).GetSession(ctx, connect.NewRequest(&browser.GetSessionRequest{}))
			require.NoError(t, err)
			require.NotNil(t, viewer.Msg)
			_, err = client.CompletePasswordLogin(ctx, connect.NewRequest(&credential.CompletePasswordLoginRequest{ChallengeId: challenge.Msg.ChallengeId, Proof: proof}))
			require.Error(t, err, "consumed challenge must never issue another session")
		})
	}
}

func TestEntryBoundaryPrivateSiteBootstrapAndAdminDenial(t *testing.T) {
	entryPrivateSite(t, true)
	server := entryBoundaryServer(t)
	client := enrollmentconnect.NewEnrollmentServiceClient(server.Client(), server.URL)
	ctx := context.Background()
	_, err := client.GetTrustBundle(ctx, connect.NewRequest(&enrollment.GetTrustBundleRequest{}))
	require.NoError(t, err)
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	started, err := client.BeginEnrollment(ctx, connect.NewRequest(&enrollment.BeginEnrollmentRequest{AgentPublicKey: &securityv1.PublicKey{Algorithm: securityv1.SignatureAlgorithm_SIGNATURE_ALGORITHM_ED25519, Value: pub}}))
	require.NoError(t, err)
	require.NotEmpty(t, started.Msg.DeviceCode)
	pending, err := client.PollEnrollment(ctx, connect.NewRequest(&enrollment.PollEnrollmentRequest{DeviceCode: "unowned-device-code"}))
	require.NoError(t, err)
	require.Equal(t, enrollment.EnrollmentState_ENROLLMENT_STATE_EXPIRED, pending.Msg.State)
	_, err = client.RefreshCredentials(ctx, connect.NewRequest(&enrollment.RefreshCredentialsRequest{}))
	require.Equal(t, connect.CodeUnauthenticated, connect.CodeOf(err))
	require.ErrorContains(t, err, "refresh", "request must reach the refresh validator")

	methods := []string{
		"/komari.enrollment.v1.EnrollmentService/RevokeCredentials",
		"/komari.enrollment.v1.EnrollmentAdminService/ListPendingEnrollments",
		"/komari.enrollment.v1.EnrollmentAdminService/GetPendingEnrollment",
		"/komari.enrollment.v1.EnrollmentAdminService/ApproveEnrollment",
		"/komari.enrollment.v1.EnrollmentAdminService/DenyEnrollment",
		"/komari.credential.v1.TrustedCredentialService/ListTrustedCredentialProviders",
		"/komari.credential.v1.TrustedCredentialService/BeginVerification",
		"/komari.credential.v1.TrustedCredentialService/CompleteVerification",
		"/komari.credential.v1.TrustedCredentialService/CancelVerification",
		"/komari.credential.v1.TrustedCredentialService/GetCredentialContacts",
		"/komari.credential.v1.TrustedCredentialService/BeginContactBinding",
		"/komari.credential.v1.TrustedCredentialService/SaveCredentialContact",
		"/komari.credential.v1.TrustedCredentialService/DeleteCredentialContact",
		"/komari.browser.v1.BrowserService/ListAgents",
		"/komari.enrollment.v1.EnrollmentService/GetTrustBundleSuffix",
	}
	for _, method := range methods {
		t.Run(method, func(t *testing.T) {
			call := connect.NewClient[emptypb.Empty, emptypb.Empty](server.Client(), server.URL+method)
			_, err := call.CallUnary(ctx, connect.NewRequest(&emptypb.Empty{}))
			require.Equal(t, connect.CodeUnauthenticated, connect.CodeOf(err))
		})
	}
}

func TestEntryBoundaryBearerErrorsPreserveRPCProtocol(t *testing.T) {
	entryPrivateSite(t, true)
	server := entryBoundaryServer(t)
	for name, options := range map[string][]connect.ClientOption{"connect": nil, "grpc": {connect.WithGRPC()}, "grpc-web": {connect.WithGRPCWeb()}} {
		t.Run(name, func(t *testing.T) {
			client := browserconnect.NewBrowserServiceClient(server.Client(), server.URL, options...)
			request := connect.NewRequest(&browser.GetPublicInfoRequest{})
			request.Header().Set("Authorization", "Bearer invalid-explicit-credential")
			_, err := client.GetPublicInfo(context.Background(), request)
			require.Equal(t, connect.CodeUnauthenticated, connect.CodeOf(err))
		})
	}
	request, err := http.NewRequest(http.MethodPost, server.URL+credentialconnect.PasswordLoginServiceBeginPasswordLoginProcedure, http.NoBody)
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer invalid-explicit-credential")
	response, err := server.Client().Do(request)
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, http.StatusUnauthorized, response.StatusCode)
}
