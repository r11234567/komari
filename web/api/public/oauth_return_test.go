package public

import "testing"

func TestSafeOAuthReturnTo(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "empty", input: "", want: "/admin"},
		{name: "bind login with nested return", input: "/bind-login?returnTo=%2Fbind%3Fcode%3DABCD", want: "/bind-login?returnTo=%2Fbind%3Fcode%3DABCD"},
		{name: "admin page", input: "/admin/servers", want: "/admin/servers"},
		{name: "protocol relative", input: "//evil.example/", want: "/admin"},
		{name: "absolute", input: "https://evil.example/", want: "/admin"},
		{name: "unrelated local path", input: "/bind?code=ABCD", want: "/admin"},
		{name: "header injection", input: "/admin\r\nLocation: https://evil.example", want: "/admin"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := safeOAuthReturnTo(tt.input); got != tt.want {
				t.Fatalf("safeOAuthReturnTo(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
