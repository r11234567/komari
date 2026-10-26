package connectapi

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/komari-monitor/komari/database/accounts"
	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/internal/credentialgateway"
	credential "github.com/komari-monitor/komari/internal/trustedcredential"
	"github.com/komari-monitor/komari/pkg/rpc"
	"github.com/pquerna/otp/totp"
	credentialv1 "github.com/r11234567/komari-proto/gen/go/komari/credential/v1"
	credentialv1connect "github.com/r11234567/komari-proto/gen/go/komari/credential/v1/credentialv1connect"
	"google.golang.org/protobuf/types/known/structpb"
)

func credentialTestIdentity(t *testing.T) (context.Context, string, string) {
	t.Helper()
	id, session := uuid.NewString(), "synthetic-credential-"+uuid.NewString()
	db := dbcore.GetDBInstance()
	if err := db.Create(&models.User{UUID: id, Username: "credential-" + id, Passwd: "synthetic-unused-password", TwoFactor: "JBSWY3DPEHPK3PXP"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&models.Session{UUID: id, Session: session, Expires: time.Now().Add(time.Hour)}).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		credentialgateway.Default.RevokeUser(id)
		db.Where("uuid = ?", id).Delete(&models.Session{})
		db.Where("uuid = ?", id).Delete(&models.User{})
	})
	ctx := rpc.NewContextWithMeta(context.Background(), &rpc.ContextMeta{Principal: rpc.NewUserPrincipal(id), UserUUID: id, SessionToken: session})
	return ctx, id, session
}

func TestCredentialContactReadReturnsOnlyMaskedPrivateRecipient(t *testing.T) {
	ctx, id, _ := credentialTestIdentity(t)
	user, err := accounts.GetUserByUUID(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := accounts.ChangeVerifiedContact(ctx, user, models.CredentialContact{Kind: "email", Address: "synthetic-private@example.test", Transport: "email"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dbcore.GetDBInstance().Where("user_uuid = ?", id).Delete(&models.CredentialContact{}) })
	result, err := (&credentialService{}).GetCredentialContacts(ctx, connect.NewRequest(&credentialv1.GetCredentialContactsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Msg.Contacts) != 1 || result.Msg.Contacts[0].MaskedAddress != "s***@example.test" || result.Msg.PasswordBootstrapAllowed {
		t.Fatal("incorrect private contact metadata")
	}
	if result.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("contact response may be cached")
	}
}

func TestCredentialGatewayNativeTOTPAndSharedInternalRPC(t *testing.T) {
	ctx, id, _ := credentialTestIdentity(t)
	svc := &credentialService{}
	providers, err := svc.ListTrustedCredentialProviders(ctx, connect.NewRequest(&credentialv1.ListTrustedCredentialProvidersRequest{}))
	if err != nil || len(providers.Msg.Providers) != 1 || providers.Msg.Providers[0].Id != "native:totp" {
		t.Fatalf("providers: %v", err)
	}
	// Start in internal RPC and finish in typed Connect: the gateway owns one
	// challenge store. No plugin or browser can supply a verification result.
	response := rpc.CallWithContext(ctx, 1, "admin:beginTrustedCredentialVerification", map[string]any{"provider_id": "native:totp", "purpose": "bind_contact", "resource": "contact:email"})
	if response.Error != nil {
		t.Fatal(response.Error)
	}
	var challenge credential.Challenge
	raw, err := json.Marshal(response.Result)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &challenge); err != nil {
		t.Fatal(err)
	}
	code, err := totp.GenerateCode("JBSWY3DPEHPK3PXP", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	proof, _ := structpb.NewStruct(map[string]any{"code": code})
	verified, err := svc.CompleteVerification(ctx, connect.NewRequest(&credentialv1.CompleteVerificationRequest{ChallengeId: challenge.ID, Proof: proof}))
	if err != nil {
		t.Fatal(err)
	}
	if verified.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("authorization may be cached")
	}
	if ok, err := accounts.Verify2Fa(id, code); err != nil || ok {
		t.Fatalf("legacy replay counter not shared: %v", err)
	}
	token := verified.Msg.Authorization.GrantToken
	if _, err := credentialgateway.Default.Consume(ctx, token, credential.BindContact, "contact:email"); err != nil {
		t.Fatal(err)
	}
	if _, err := credentialgateway.Default.Consume(ctx, token, credential.BindContact, "contact:email"); !errors.Is(err, credential.ErrNotFound) {
		t.Fatalf("grant reused: %v", err)
	}
}

func TestCredentialSessionAndSecurityChangesInvalidatePendingWork(t *testing.T) {
	for _, change := range []string{"password", "totp", "logout"} {
		t.Run(change, func(t *testing.T) {
			ctx, id, session := credentialTestIdentity(t)
			c, err := credentialgateway.Default.Begin(ctx, credential.Request{ProviderID: "native:totp", Purpose: credential.LoginSettings, Resource: "account"})
			if err != nil {
				t.Fatal(err)
			}
			db := dbcore.GetDBInstance()
			switch change {
			case "password":
				err = db.Model(&models.User{}).Where("uuid = ?", id).Update("passwd", "synthetic-replaced").Error
			case "totp":
				err = accounts.Disable2Fa(id)
			case "logout":
				err = accounts.DeleteSession(session)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := credentialgateway.Default.Complete(ctx, c.ID, json.RawMessage(`{"code":"000000"}`)); err == nil {
				t.Fatal("changed security state retained challenge")
			}
		})
	}
}

func TestCredentialServicePolicyAndHTTPAuthentication(t *testing.T) {
	prefix := "/komari.credential.v1.TrustedCredentialService/"
	for _, method := range []string{"ListTrustedCredentialProviders", "BeginVerification", "CompleteVerification", "CancelVerification", "GetCredentialContacts", "BeginContactBinding", "SaveCredentialContact", "DeleteCredentialContact"} {
		p, ok := newPolicyInterceptor().policies[prefix+method]
		if !ok || p.role != rpc.RoleAdmin || p.maxDuration != 30*time.Second {
			t.Fatalf("missing policy for %s", method)
		}
	}
	client := credentialv1connect.NewTrustedCredentialServiceClient(testServer.Client(), testServer.URL)
	if _, err := client.ListTrustedCredentialProviders(context.Background(), connect.NewRequest(&credentialv1.ListTrustedCredentialProvidersRequest{})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("anonymous HTTP: %v", err)
	}
	ctx, _, session := credentialTestIdentity(t)
	req := connect.NewRequest(&credentialv1.ListTrustedCredentialProvidersRequest{})
	req.Header().Set("Cookie", "session_token="+session)
	listed, err := client.ListTrustedCredentialProviders(context.Background(), req)
	if err != nil || len(listed.Msg.Providers) != 1 {
		t.Fatalf("interactive HTTP: %v", err)
	}
	if listed.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("provider metadata may be cached")
	}
	m := rpc.MetaFromContext(ctx)
	for _, p := range []*rpc.Principal{rpc.NewAPIKeyPrincipal(), rpc.PrincipalFromRole(rpc.RoleAdmin)} {
		other := rpc.NewContextWithMeta(context.Background(), &rpc.ContextMeta{Principal: p, UserUUID: m.UserUUID, SessionToken: session})
		if _, err := (&credentialService{}).ListTrustedCredentialProviders(other, connect.NewRequest(&credentialv1.ListTrustedCredentialProvidersRequest{})); connect.CodeOf(err) != connect.CodeUnauthenticated {
			t.Fatalf("noninteractive context: %v", err)
		}
	}
}
