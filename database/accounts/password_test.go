package accounts

import (
	"strings"
	"testing"
)

func TestPasswordHashUsesBcrypt(t *testing.T) {
	hashed, err := hashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(hashed, "$2") {
		t.Fatalf("expected bcrypt hash, got %q", hashed)
	}
	if valid, legacy := verifyPasswordHash(hashed, "correct horse battery staple"); !valid || legacy {
		t.Fatalf("bcrypt password verification returned valid=%t legacy=%t", valid, legacy)
	}
	if valid, _ := verifyPasswordHash(hashed, "wrong password"); valid {
		t.Fatal("wrong password unexpectedly matched bcrypt hash")
	}
}

func TestLegacyPasswordHashRemainsVerifiableForMigration(t *testing.T) {
	legacyHash := legacyPasswordHash("legacy password")
	if valid, legacy := verifyPasswordHash(legacyHash, "legacy password"); !valid || !legacy {
		t.Fatalf("legacy password verification returned valid=%t legacy=%t", valid, legacy)
	}
	if valid, _ := verifyPasswordHash(legacyHash, "wrong password"); valid {
		t.Fatal("wrong password unexpectedly matched legacy hash")
	}
}
