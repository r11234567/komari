package factory

import "testing"

func TestCredentialAddressIsOnePrivateRecipient(t *testing.T) {
	for _, tc := range []struct{ kind, input, want string }{
		{"email", "user@Example.TEST", "user@example.test"}, {"telegram", "123456789", "123456789"},
		{"email", "two@example.test,other@example.test", ""}, {"email", "Name <user@example.test>", ""},
		{"email", "user@example.test\r\nBcc: other@example.test", ""}, {"telegram", "-1234", ""}, {"telegram", "@publicgroup", ""}, {"telegram", "01234", ""}, {"telegram", "0", ""}, {"other", "1234", ""},
	} {
		got, err := NormalizeCredentialAddress(tc.kind, tc.input)
		if tc.want == "" {
			if err == nil {
				t.Fatal("non-private address accepted")
			}
		} else if err != nil || got != tc.want {
			t.Fatalf("address normalization: %v", err)
		}
	}
}
