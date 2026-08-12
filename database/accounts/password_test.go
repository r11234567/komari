package accounts

import (
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func TestNewPasswordHashUsesBcrypt(t *testing.T) {
	hashed, err := hashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if hashed == legacyPasswordHash("correct horse battery staple") {
		t.Fatal("new password used the legacy hash")
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hashed), []byte("correct horse battery staple")); err != nil {
		t.Fatalf("bcrypt verification failed: %v", err)
	}
	if bcrypt.CompareHashAndPassword([]byte(hashed), []byte("wrong")) == nil {
		t.Fatal("bcrypt accepted an incorrect password")
	}
}
