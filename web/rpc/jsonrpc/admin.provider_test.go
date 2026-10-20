package jsonrpc

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMaskSecretString(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"", ""},
		{"abc", "●●●"},
		{"1234567890", "●●●●●●●●●●"},
		{"secret_token_123", strings.Repeat("●", 16)},
	}

	for _, tc := range tests {
		got := maskSecretString(tc.input)
		if got != tc.expected {
			t.Errorf("maskSecretString(%q) = %q, want %q", tc.input, got, tc.expected)
		}
	}
}

func TestIsMaskedSecret(t *testing.T) {
	if isMaskedSecret("") {
		t.Errorf("isMaskedSecret(\"\") should be false")
	}
	if !isMaskedSecret("●●●●●") {
		t.Errorf("isMaskedSecret(\"●●●●●\") should be true")
	}
	if !isMaskedSecret("*****") {
		t.Errorf("isMaskedSecret(\"*****\") should be true")
	}
	if isMaskedSecret("●●●abc") {
		t.Errorf("isMaskedSecret(\"●●●abc\") should be false")
	}
	if isMaskedSecret("my_secret_token") {
		t.Errorf("isMaskedSecret(\"my_secret_token\") should be false")
	}
}

func TestMaskAdditionSecrets(t *testing.T) {
	secretFields := map[string]bool{
		"bot_token": true,
	}
	input := `{"bot_token":"123456:ABC","chat_id":"987654"}`
	masked := maskAdditionSecrets("telegram", input, secretFields)

	var data map[string]any
	if err := json.Unmarshal([]byte(masked), &data); err != nil {
		t.Fatalf("failed to unmarshal masked json: %v", err)
	}

	if data["chat_id"] != "987654" {
		t.Errorf("chat_id should not be masked, got %v", data["chat_id"])
	}
	expectedDots := strings.Repeat("●", len("123456:ABC"))
	if data["bot_token"] != expectedDots {
		t.Errorf("bot_token masked = %v, want %v", data["bot_token"], expectedDots)
	}
}

func TestRestoreAdditionSecrets(t *testing.T) {
	secretFields := map[string]bool{
		"bot_token": true,
	}
	oldJSON := `{"bot_token":"super-secret-123","chat_id":"111"}`

	// Case 1: Frontend sends masked dots back unchanged (e.g. user only changed chat_id)
	incomingMasked := `{"bot_token":"●●●●●●●●●●●●●●●●","chat_id":"222"}`
	restored := restoreAdditionSecrets(incomingMasked, oldJSON, secretFields)

	var data map[string]any
	if err := json.Unmarshal([]byte(restored), &data); err != nil {
		t.Fatalf("failed to unmarshal restored json: %v", err)
	}
	if data["bot_token"] != "super-secret-123" {
		t.Errorf("bot_token should have been restored to super-secret-123, got %v", data["bot_token"])
	}
	if data["chat_id"] != "222" {
		t.Errorf("chat_id should be updated to 222, got %v", data["chat_id"])
	}

	// Case 2: Frontend sends a brand new secret
	incomingNew := `{"bot_token":"brand-new-secret","chat_id":"222"}`
	restoredNew := restoreAdditionSecrets(incomingNew, oldJSON, secretFields)

	var dataNew map[string]any
	if err := json.Unmarshal([]byte(restoredNew), &dataNew); err != nil {
		t.Fatalf("failed to unmarshal restored json: %v", err)
	}
	if dataNew["bot_token"] != "brand-new-secret" {
		t.Errorf("bot_token should be brand-new-secret, got %v", dataNew["bot_token"])
	}
}
