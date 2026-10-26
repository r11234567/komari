package connectapi

import (
	"context"
	"net/http"
	"testing"

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
