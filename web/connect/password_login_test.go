package connectapi

import (
	"context"
	"github.com/pquerna/otp/totp"
	credentialv1connect "github.com/r11234567/komari-proto/gen/go/komari/credential/v1/credentialv1connect"
	"google.golang.org/protobuf/types/known/structpb"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/komari-monitor/komari/database/accounts"
	credentialv1 "github.com/r11234567/komari-proto/gen/go/komari/credential/v1"
)

func TestPasswordLoginConnectCookiesAndResponseMetadata(t *testing.T) {
	user, err := accounts.CreateAccount("synthetic-password-login-"+uuid.NewString(), "Synthetic-password-123")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = accounts.DeleteAccountByUsername(user.Username) })
	if err := accounts.Enable2Fa(user.UUID, "JBSWY3DPEHPK3PXP"); err != nil {
		t.Fatal(err)
	}
	service := &passwordLoginService{}
	response, err := service.BeginPasswordLogin(context.Background(), connect.NewRequest(&credentialv1.BeginPasswordLoginRequest{Username: user.Username, Password: "Synthetic-password-123"}))
	if err != nil || response.Msg.Authenticated || len(response.Msg.Providers) != 1 {
		t.Fatal("invalid password-stage response", err)
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("login response cacheable")
	}
	cookies := (&http.Response{Header: response.Header()}).Cookies()
	if len(cookies) != 1 || cookies[0].Name != passwordCookieName || !cookies[0].HttpOnly || !cookies[0].Secure || cookies[0].SameSite != http.SameSiteStrictMode || cookies[0].Path != passwordServicePath {
		t.Fatal("invalid preauthentication cookie attributes")
	}
	cancel := connect.NewRequest(&credentialv1.CancelPasswordLoginRequest{})
	cancel.Header().Add("Cookie", cookies[0].String())
	result, err := service.CancelPasswordLogin(context.Background(), cancel)
	if err != nil || !result.Msg.Cancelled {
		t.Fatal("cookie-bound cancellation failed", err)
	}
}

// Actual HTTPS/Connect requests exercise cookie Path/Secure and cross-document
// navigation. No frontend marker or password replay is supplied by page two.
func TestPasswordLoginCrossDocumentCookieResume(t *testing.T) {
	user, err := accounts.CreateAccount("synthetic-navigation-"+uuid.NewString(), "Synthetic-password-123")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = accounts.DeleteAccountByUsername(user.Username) })
	const secret = "JBSWY3DPEHPK3PXP"
	if err := accounts.Enable2Fa(user.UUID, secret); err != nil {
		t.Fatal(err)
	}
	path, handler := credentialv1connect.NewPasswordLoginServiceHandler(&passwordLoginService{})
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	mux.HandleFunc("/login/verify", func(w http.ResponseWriter, r *http.Request) {
		if _, err := r.Cookie(passwordCookieName); err == nil {
			t.Error("service cookie leaked into document request")
		}
		w.WriteHeader(http.StatusOK)
	})
	server := httptest.NewTLSServer(mux)
	defer server.Close()
	httpClient := server.Client()
	httpClient.Jar, _ = cookiejar.New(nil)
	client := credentialv1connect.NewPasswordLoginServiceClient(httpClient, server.URL)
	if _, err := client.BeginLoginVerification(context.Background(), connect.NewRequest(&credentialv1.BeginLoginVerificationRequest{})); err == nil {
		t.Fatal("missing password accepted")
	}
	started, err := client.BeginPasswordLogin(context.Background(), connect.NewRequest(&credentialv1.BeginPasswordLoginRequest{Username: user.Username, Password: "Synthetic-password-123"}))
	if err != nil || started.Msg.Authenticated {
		t.Fatal("password phase failed", err)
	}
	response, err := httpClient.Get(server.URL + "/login/verify")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	first, err := client.BeginLoginVerification(context.Background(), connect.NewRequest(&credentialv1.BeginLoginVerificationRequest{}))
	if err != nil || first.Msg.PromptType != "otp" {
		t.Fatal("cookie did not restore TOTP fallback", err)
	}
	again, err := client.BeginLoginVerification(context.Background(), connect.NewRequest(&credentialv1.BeginLoginVerificationRequest{}))
	if err != nil || again.Msg.ChallengeId != first.Msg.ChallengeId {
		t.Fatal("refresh replaced challenge", err)
	}
	code, err := totp.GenerateCode(secret, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	proof, _ := structpb.NewStruct(map[string]any{"code": code})
	completed, err := client.CompletePasswordLogin(context.Background(), connect.NewRequest(&credentialv1.CompletePasswordLoginRequest{ChallengeId: first.Msg.ChallengeId, Proof: proof}))
	if err != nil || !completed.Msg.Authenticated {
		t.Fatal("resumed flow did not authenticate", err)
	}
	root, _ := url.Parse(server.URL + "/admin/dashboard")
	var authenticated bool
	for _, cookie := range httpClient.Jar.Cookies(root) {
		if _, _, err := accounts.GetSessionIdentity(cookie.Value); err == nil {
			authenticated = true
		}
	}
	if !authenticated {
		t.Fatal("full session cookie not available to dashboard")
	}
	if _, err := client.CompletePasswordLogin(context.Background(), connect.NewRequest(&credentialv1.CompletePasswordLoginRequest{ChallengeId: first.Msg.ChallengeId, Proof: proof})); err == nil {
		t.Fatal("consumed proof replayed")
	}
}
