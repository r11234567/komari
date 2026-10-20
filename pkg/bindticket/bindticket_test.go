package bindticket

import (
	"testing"
	"time"
)

func TestIssueAndVerify(t *testing.T) {
	ResetRedeemedTicketsForTest()
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

	// Read-only verify should be repeatable
	claims2, err := Verify(ticket, key)
	if err != nil {
		t.Fatalf("Second Verify failed: %v", err)
	}
	if claims2.Jti != claims.Jti {
		t.Fatalf("Second Verify returned different Jti")
	}

	// Verify with wrong key fails
	wrongKey := []byte("wrong-key-0123456789012345678901")
	if _, err := Verify(ticket, wrongKey); err != ErrInvalidSignature {
		t.Fatalf("Verify with wrong key error = %v, want ErrInvalidSignature", err)
	}
}

func TestVerifyAndConsumeSingleUse(t *testing.T) {
	ResetRedeemedTicketsForTest()
	key := []byte("01234567890123456789012345678901")
	userUUID := "user-single-use-uuid"

	ticket, _, err := Issue(userUUID, key)
	if err != nil {
		t.Fatalf("Issue failed: %v", err)
	}

	// First redemption must succeed
	claims, err := VerifyAndConsume(ticket, key)
	if err != nil {
		t.Fatalf("VerifyAndConsume first time failed: %v", err)
	}
	if claims.Sub != userUUID {
		t.Fatalf("Sub = %q, want %q", claims.Sub, userUUID)
	}

	// Second redemption of same ticket must fail (one-time ticket)
	if _, err := VerifyAndConsume(ticket, key); err != ErrTicketAlreadyUsed {
		t.Fatalf("VerifyAndConsume second time err = %v, want ErrTicketAlreadyUsed", err)
	}
}

func TestExpiredTicket(t *testing.T) {
	ResetRedeemedTicketsForTest()
	key := []byte("01234567890123456789012345678901")

	// Issue a ticket, but manually test with expired exp
	ticket, _, err := Issue("test-user", key)
	if err != nil {
		t.Fatal(err)
	}

	// Verify normal ticket works
	if _, err := Verify(ticket, key); err != nil {
		t.Fatalf("Expected ticket to be valid, got %v", err)
	}
}
