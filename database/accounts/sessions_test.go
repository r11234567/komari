package accounts

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/komari-monitor/komari/cmd/flags"
	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/models"
)

func TestMain(m *testing.M) {
	root, err := os.MkdirTemp("", "komari-accounts-test-*")
	if err != nil {
		panic(err)
	}
	flags.DatabaseType = flags.DatabaseTypeSQLite
	flags.DatabaseFile = filepath.Join(root, "accounts.db")
	if err := dbcore.Initialize(); err != nil {
		panic(err)
	}
	code := m.Run()
	_ = dbcore.Close()
	_ = os.RemoveAll(root)
	os.Exit(code)
}

func TestSessionDigestDomainSeparationAndStrictDeleteByID(t *testing.T) {
	user, err := CreateAccount("sess-domain-"+uuid.NewString(), "Strong-Password-123")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db := dbcore.GetDBInstance()
		db.Where("uuid = ?", user.UUID).Delete(&models.Session{})
		db.Where("uuid = ?", user.UUID).Delete(&models.User{})
	})

	cookie, err := CreateSession(user.UUID, 3600, "test-ua", "127.0.0.1", "password")
	if err != nil {
		t.Fatal(err)
	}
	storedID := SessionIDForCookie(cookie)
	if len(storedID) != 67 || !strings.HasPrefix(storedID, "v1:") {
		t.Fatalf("unexpected stored session digest: %q", storedID)
	}
	// Passing a stored digest where a raw browser cookie is expected must fail domain separation.
	if SessionIDForCookie(storedID) != "" {
		t.Fatal("SessionIDForCookie accepted a v1: digest")
	}
	if _, err := GetSession(storedID); err == nil {
		t.Fatal("GetSession accepted a stored v1: digest as a browser cookie")
	}
	// Passing raw cookie or uppercase hex to DeleteSessionByID must fail with ErrInvalidSessionID.
	if err := DeleteSessionByID(cookie); !errors.Is(err, ErrInvalidSessionID) {
		t.Fatalf("DeleteSessionByID(raw cookie) = %v, want ErrInvalidSessionID", err)
	}
	if err := DeleteSessionByID("v1:" + strings.ToUpper(storedID[3:])); !errors.Is(err, ErrInvalidSessionID) {
		t.Fatalf("DeleteSessionByID(uppercase hex) = %v, want ErrInvalidSessionID", err)
	}
	// Valid lookup by raw cookie succeeds.
	gotUUID, err := GetSession(cookie)
	if err != nil || gotUUID != user.UUID {
		t.Fatalf("GetSession(cookie) = (%q, %v), want %q", gotUUID, err, user.UUID)
	}
	// Delete by stored ID succeeds and revokes cookie.
	if err := DeleteSessionByID(storedID); err != nil {
		t.Fatalf("DeleteSessionByID(valid) failed: %v", err)
	}
	if _, err := GetSession(cookie); err == nil {
		t.Fatal("session still authenticated after DeleteSessionByID")
	}
}

func TestSessionExpiryHandlesNonUTCTimezoneOffsets(t *testing.T) {
	user, err := CreateAccount("sess-tz-"+uuid.NewString(), "Strong-Password-123")
	if err != nil {
		t.Fatal(err)
	}
	db := dbcore.GetDBInstance()
	t.Cleanup(func() {
		db.Where("uuid = ?", user.UUID).Delete(&models.Session{})
		db.Where("uuid = ?", user.UUID).Delete(&models.User{})
	})

	// 1. Expired row stored with +08:00 timezone offset (lexicographically AFTER current UTC string!).
	posZone := time.FixedZone("UTC+8", 8*3600)
	expiredCookie := "expired-pos-tz-" + uuid.NewString()
	expiredID := SessionIDForCookie(expiredCookie)
	if err := db.Create(&models.Session{
		UUID:    user.UUID,
		Session: expiredID,
		Expires: time.Now().In(posZone).Add(-5 * time.Minute),
	}).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := GetSession(expiredCookie); err == nil {
		t.Fatal("expired +08:00 session authenticated via lexicographic timestamp bug")
	}
	if _, err := GetUserBySession(expiredCookie); err == nil {
		t.Fatal("GetUserBySession accepted expired +08:00 session")
	}
	if _, _, err := GetSessionIdentity(expiredCookie); err == nil {
		t.Fatal("GetSessionIdentity accepted expired +08:00 session")
	}

	// 2. Unexpired row stored with -07:00 timezone offset (lexicographically BEFORE current UTC string!).
	negZone := time.FixedZone("UTC-7", -7*3600)
	validCookie := "valid-neg-tz-" + uuid.NewString()
	validID := SessionIDForCookie(validCookie)
	if err := db.Create(&models.Session{
		UUID:    user.UUID,
		Session: validID,
		Expires: time.Now().In(negZone).Add(30 * time.Minute),
	}).Error; err != nil {
		t.Fatal(err)
	}
	if got, err := GetSession(validCookie); err != nil || got != user.UUID {
		t.Fatalf("valid -07:00 session rejected: (%q, %v)", got, err)
	}
	if err := UpdateLatest(validCookie, "ua-2", "198.51.100.5"); err != nil {
		t.Fatalf("UpdateLatest failed for valid -07:00 session: %v", err)
	}

	// 3. GetAllSessions omits expired rows and retains valid ones.
	all, err := GetAllSessions()
	if err != nil {
		t.Fatal(err)
	}
	sawValid, sawExpired := false, false
	for _, s := range all {
		if s.Session == validID {
			sawValid = true
		}
		if s.Session == expiredID {
			sawExpired = true
		}
	}
	if !sawValid || sawExpired {
		t.Fatalf("GetAllSessions sawValid=%v sawExpired=%v, want true/false", sawValid, sawExpired)
	}

	// 4. RemoveExpiredSessions purges the +08:00 expired row while keeping the -07:00 valid row.
	if err := RemoveExpiredSessions(); err != nil {
		t.Fatal(err)
	}
	var expiredCount, validCount int64
	db.Model(&models.Session{}).Where("session = ?", expiredID).Count(&expiredCount)
	db.Model(&models.Session{}).Where("session = ?", validID).Count(&validCount)
	if expiredCount != 0 || validCount != 1 {
		t.Fatalf("post-purge counts expired=%d valid=%d, want 0/1", expiredCount, validCount)
	}
}

func TestSessionInvalidatedWhenSecurityEpochAdvances(t *testing.T) {
	t.Cleanup(func() { dbcore.SetCurrentSecurityEpoch(1) })
	user, err := CreateAccount("sess-epoch-"+uuid.NewString(), "Strong-Password-123")
	if err != nil {
		t.Fatal(err)
	}
	db := dbcore.GetDBInstance()
	t.Cleanup(func() {
		db.Where("uuid = ?", user.UUID).Delete(&models.Session{})
		db.Where("uuid = ?", user.UUID).Delete(&models.User{})
	})

	dbcore.SetCurrentSecurityEpoch(1)
	oldSession, err := CreateSession(user.UUID, 3600, "ua-1", "127.0.0.1", "password")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := GetSession(oldSession); err != nil || got != user.UUID {
		t.Fatalf("epoch-1 session failed before epoch change: (%q, %v)", got, err)
	}

	dbcore.SetCurrentSecurityEpoch(2)
	if _, err := GetSession(oldSession); err == nil {
		t.Fatal("stale epoch-1 session remained valid after security epoch advanced to 2")
	}
	if _, _, err := GetSessionIdentity(oldSession); err == nil {
		t.Fatal("GetSessionIdentity accepted stale epoch-1 session")
	}
	if _, err := GetUserBySession(oldSession); err == nil {
		t.Fatal("GetUserBySession accepted stale epoch-1 session")
	}
	all, err := GetAllSessions()
	if err != nil {
		t.Fatal(err)
	}
	oldID := SessionIDForCookie(oldSession)
	for _, s := range all {
		if s.Session == oldID {
			t.Fatal("GetAllSessions returned stale epoch-1 session")
		}
	}

	newSession, err := CreateSession(user.UUID, 3600, "ua-2", "127.0.0.1", "password")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := GetSession(newSession); err != nil || got != user.UUID {
		t.Fatalf("epoch-2 session failed: (%q, %v)", got, err)
	}
}
