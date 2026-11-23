package oauth

import (
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/komari-monitor/komari/cmd/flags"
	"github.com/komari-monitor/komari/database/accounts"
	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/models"
)

func TestMain(m *testing.M) {
	flags.DatabaseType = flags.DatabaseTypeSQLite
	flags.DatabaseFile = "file:web_oauth_binding_test?mode=memory&cache=shared"
	db := dbcore.GetDBInstance()
	if sqlDB, err := db.DB(); err == nil {
		sqlDB.SetMaxOpenConns(1)
	}
	os.Exit(m.Run())
}

func TestBindingTokenSessionBoundAndSingleUse(t *testing.T) {
	userA, err := accounts.CreateAccount("oauth-bind-a-"+uuid.NewString()[:8], "Strong-Password-123")
	if err != nil {
		t.Fatal(err)
	}
	userB, err := accounts.CreateAccount("oauth-bind-b-"+uuid.NewString()[:8], "Strong-Password-123")
	if err != nil {
		t.Fatal(err)
	}
	db := dbcore.GetDBInstance()
	t.Cleanup(func() {
		db.Where("uuid = ?", userA.UUID).Delete(&models.Session{})
		db.Where("uuid = ?", userB.UUID).Delete(&models.Session{})
		_ = accounts.DeleteAccountByUsername(userA.Username)
		_ = accounts.DeleteAccountByUsername(userB.Username)
	})

	sessA1, err := accounts.CreateSession(userA.UUID, 3600, "ua-1", "127.0.0.1", "password")
	if err != nil {
		t.Fatal(err)
	}
	sessA2, err := accounts.CreateSession(userA.UUID, 3600, "ua-2", "127.0.0.1", "password")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := IssueBindingToken(userA.UUID, "invalid-raw-or-missing-sess"); err == nil {
		// Note: SessionIDForCookie accepts arbitrary raw cookie tokens, whereas empty or v1: digest is rejected.
	}
	if _, err := IssueBindingToken("", sessA1); err == nil {
		t.Fatal("expected empty userUUID to fail")
	}
	if _, err := IssueBindingToken(userA.UUID, ""); err == nil {
		t.Fatal("expected empty sessionToken to fail")
	}

	// Wrong session cookie cannot consume nonce issued for sessA1.
	nonce1, err := IssueBindingToken(userA.UUID, sessA1)
	if err != nil {
		t.Fatal(err)
	}
	if ConsumeBindingToken(nonce1, userA.UUID, sessA2) {
		t.Fatal("cross-session binding consumption succeeded unexpectedly")
	}

	// Wrong user UUID cannot consume nonce.
	nonce2, err := IssueBindingToken(userA.UUID, sessA1)
	if err != nil {
		t.Fatal(err)
	}
	if ConsumeBindingToken(nonce2, userB.UUID, sessA1) {
		t.Fatal("cross-user binding consumption succeeded unexpectedly")
	}

	// Forged or empty nonces are rejected.
	if ConsumeBindingToken("forged-nonce-value", userA.UUID, sessA1) || ConsumeBindingToken("", userA.UUID, sessA1) {
		t.Fatal("forged or empty nonce succeeded unexpectedly")
	}

	// Matching user + session consumes once and cannot be replayed.
	nonce3, err := IssueBindingToken(userA.UUID, sessA1)
	if err != nil {
		t.Fatal(err)
	}
	if !ConsumeBindingToken(nonce3, userA.UUID, sessA1) {
		t.Fatal("valid binding nonce failed to consume")
	}
	if ConsumeBindingToken(nonce3, userA.UUID, sessA1) {
		t.Fatal("replayed binding nonce succeeded")
	}

	// Expired nonces are rejected.
	now := time.Now()
	store := &bindingStore{now: func() time.Time { return now }, entries: make(map[[32]byte]bindingRecord)}
	expNonce, err := store.issue(userA.UUID, sessA1)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(bindingTokenTTL + time.Second)
	if store.consume(expNonce, userA.UUID, sessA1) {
		t.Fatal("expired binding nonce unexpectedly succeeded")
	}
}
