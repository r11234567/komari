package api

import (
	"testing"
	"time"
)

func TestSensitive2FACodeCannotBeReplayed(t *testing.T) {
	now := time.Now()
	if !claimSensitive2FACode("user-a", "123456", now) {
		t.Fatal("first 2FA code use was rejected")
	}
	if claimSensitive2FACode("user-a", "123456", now.Add(time.Second)) {
		t.Fatal("replayed 2FA code was accepted")
	}
	if !claimSensitive2FACode("user-b", "123456", now.Add(time.Second)) {
		t.Fatal("another user's code was incorrectly treated as replay")
	}
}
