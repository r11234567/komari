package connectapi

import (
	"errors"
	"net/http"
	"testing"

	"connectrpc.com/connect"
	"github.com/komari-monitor/komari/database/accounts"
	"github.com/komari-monitor/komari/pkg/bindticket"
	"github.com/komari-monitor/komari/pkg/rpc"
	"github.com/komari-monitor/komari/web/security"
)

func TestVerifyBindTicketIsReusableAndSubjectBound(t *testing.T) {
	const userUUID = "bind-ticket-test-user"
	key := bindticket.SessionKey(accounts.SessionDigestKey(), "test-console-session")
	ticket, _, err := bindticket.Issue(userUUID, key)
	if err != nil {
		t.Fatalf("Issue failed: %v", err)
	}

	header := http.Header{}
	(&http.Request{Header: header}).AddCookie(&http.Cookie{Name: security.BindTicketCookieName, Value: ticket})
	meta := &rpc.ContextMeta{Principal: rpc.NewUserPrincipal(userUUID), SessionToken: "test-console-session"}

	claims, err := verifyBindTicket(header, meta)
	if err != nil {
		t.Fatalf("first verification failed: %v", err)
	}
	if claims.Sub != userUUID {
		t.Fatalf("claims.Sub = %q, want %q", claims.Sub, userUUID)
	}

	claimsAgain, err := verifyBindTicket(header, meta)
	if err != nil {
		t.Fatalf("ticket must remain reusable within its lifetime: %v", err)
	}
	if claimsAgain.Jti != claims.Jti {
		t.Fatalf("repeated verification changed Jti")
	}

	if _, err := verifyBindTicket(header, &rpc.ContextMeta{Principal: meta.Principal, SessionToken: "another-session"}); err == nil {
		t.Fatal("another session of the same administrator accepted the ticket")
	}

	_, err = verifyBindTicket(header, &rpc.ContextMeta{Principal: rpc.NewUserPrincipal("someone-else"), SessionToken: meta.SessionToken})
	if err == nil {
		t.Fatal("subject mismatch was accepted")
	}
	connectErr := bindTicketConnectError(err)
	var detailed *connect.Error
	if !errors.As(connectErr, &detailed) {
		t.Fatalf("expected connect error, got %T", connectErr)
	}
	if detailed.Code() != connect.CodePermissionDenied {
		t.Fatalf("code = %v, want permission denied", detailed.Code())
	}
	if got := detailed.Meta().Get(bindTicketReasonHeader); got != "bind_ticket_subject_mismatch" {
		t.Fatalf("reason = %q, want bind_ticket_subject_mismatch", got)
	}
}

func TestVerifyBindTicketRequiresCookieAndAdminSession(t *testing.T) {
	if _, err := verifyBindTicket(http.Header{}, &rpc.ContextMeta{Principal: rpc.NewUserPrincipal("user"), SessionToken: "test-console-session"}); err == nil {
		t.Fatal("missing ticket was accepted")
	}

	key := bindticket.SessionKey(accounts.SessionDigestKey(), "test-console-session")
	ticket, _, err := bindticket.Issue("user", key)
	if err != nil {
		t.Fatalf("Issue failed: %v", err)
	}
	header := http.Header{}
	(&http.Request{Header: header}).AddCookie(&http.Cookie{Name: security.BindTicketCookieName, Value: ticket})
	if _, err := verifyBindTicket(header, &rpc.ContextMeta{Principal: rpc.NewAnonymousPrincipal()}); err == nil {
		t.Fatal("anonymous principal was accepted")
	}
}
