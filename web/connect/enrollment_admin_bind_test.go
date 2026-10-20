package connectapi

import (
	"errors"
	"net/http"
	"testing"

	"connectrpc.com/connect"
	"github.com/komari-monitor/komari/database/accounts"
	"github.com/komari-monitor/komari/pkg/bindticket"
	"github.com/komari-monitor/komari/pkg/rpc"
)

func TestVerifyBindTicketIsReusableAndSubjectBound(t *testing.T) {
	const userUUID = "bind-ticket-test-user"
	key := accounts.SessionDigestKey()
	ticket, _, err := bindticket.Issue(userUUID, key)
	if err != nil {
		t.Fatalf("Issue failed: %v", err)
	}

	header := http.Header{}
	header.Set("X-Bind-Ticket", ticket)
	principal := rpc.NewUserPrincipal(userUUID)

	claims, err := verifyBindTicket(header, principal)
	if err != nil {
		t.Fatalf("first verification failed: %v", err)
	}
	if claims.Sub != userUUID {
		t.Fatalf("claims.Sub = %q, want %q", claims.Sub, userUUID)
	}

	claimsAgain, err := verifyBindTicket(header, principal)
	if err != nil {
		t.Fatalf("ticket must remain reusable within its lifetime: %v", err)
	}
	if claimsAgain.Jti != claims.Jti {
		t.Fatalf("repeated verification changed Jti")
	}

	_, err = verifyBindTicket(header, rpc.NewUserPrincipal("someone-else"))
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

func TestVerifyBindTicketRequiresHeaderAndAdminPrincipal(t *testing.T) {
	if _, err := verifyBindTicket(http.Header{}, rpc.NewUserPrincipal("user")); err == nil {
		t.Fatal("missing ticket was accepted")
	}

	key := accounts.SessionDigestKey()
	ticket, _, err := bindticket.Issue("user", key)
	if err != nil {
		t.Fatalf("Issue failed: %v", err)
	}
	header := http.Header{}
	header.Set("X-Bind-Ticket", ticket)
	if _, err := verifyBindTicket(header, rpc.NewAnonymousPrincipal()); err == nil {
		t.Fatal("anonymous principal was accepted")
	}
}
