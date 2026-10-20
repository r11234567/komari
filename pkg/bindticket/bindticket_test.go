package bindticket

import (
	"testing"
	"time"
)

func TestIssueAndVerify(t *testing.T) {
	key := []byte("01234567890123456789012345678901")
	userUUID := "user-1234-uuid"

	ticket, exp, err := Issue(userUUID, key)
	if err != nil {
		t.Fatalf("Issue failed: %v", err)
	}
	if ticket == "" {
		t.Fatal("Issued ticket is empty")
	}
	if exp <= time.Now().Unix() {
		t.Fatalf("Issued ticket has invalid exp: %d", exp)
	}

	// Verify (read-only)
	claims, err := Verify(ticket, key)
	if err != nil {
		t.Fatalf("Verify failed: %v", err)
	}
	if claims.Sub != userUUID {
		t.Fatalf("Claims Sub = %q, want %q", claims.Sub, userUUID)
	}
	if claims.Type != TicketType {
		t.Fatalf("Claims Type = %q, want %q", claims.Type, TicketType)
	}
	if claims.Aud != TicketAudience {
		t.Fatalf("Claims Aud = %q, want %q", claims.Aud, TicketAudience)
	}
	if claims.Scope != TicketScope {
		t.Fatalf("Claims Scope = %q, want %q", claims.Scope, TicketScope)
	}
	if claims.Jti == "" {
		t.Fatal("Claims Jti is empty")
	}

	for attempt := 2; attempt <= 4; attempt++ {
		claimsN, err := Verify(ticket, key)
		if err != nil {
			t.Fatalf("Verify attempt %d failed: %v", attempt, err)
		}
		if claimsN.Jti != claims.Jti {
			t.Fatalf("Verify attempt %d returned different Jti", attempt)
		}
	}

	// Verify with wrong key fails
	wrongKey := []byte("wrong-key-0123456789012345678901")
	if _, err := Verify(ticket, wrongKey); err != ErrInvalidSignature {
		t.Fatalf("Verify with wrong key error = %v, want ErrInvalidSignature", err)
	}
}

func TestExpiredTicket(t *testing.T) {
	key := []byte("01234567890123456789012345678901")
	ticket, _, err := issueAt("expired-user", key, time.Now().UTC().Add(-TicketDuration-time.Second))
	if err != nil {
		t.Fatalf("Issue failed: %v", err)
	}
	if _, err := Verify(ticket, key); err != ErrTicketExpired {
		t.Fatalf("Verify expired ticket error = %v, want ErrTicketExpired", err)
	}
}

func TestIssueRejectsMissingKeyAndSubject(t *testing.T) {
	key := []byte("01234567890123456789012345678901")
	if _, _, err := Issue("user", nil); err != ErrMissingKey {
		t.Fatalf("Issue with missing key error = %v, want ErrMissingKey", err)
	}
	if _, _, err := Issue("", key); err != ErrInvalidToken {
		t.Fatalf("Issue with empty subject error = %v, want ErrInvalidToken", err)
	}
}
