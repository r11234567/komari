package github

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/komari-monitor/komari/web/oauth/factory"
)

func TestGitHubOnCallbackConsumesStateOnceAndRejectsOversizedResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"gho_test_token"}`))
		case "/user":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":42,"login":"alice"}`))
		case "/oversized":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"`))
			_, _ = w.Write([]byte(strings.Repeat("x", int(factory.MaxResponseBodyBytes)+64)))
			_, _ = w.Write([]byte(`"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	provider := &Github{
		Addition: Addition{ClientId: "cid", ClientSecret: "csec"},
		tokenURL: srv.URL + "/token",
		userURL:  srv.URL + "/user",
	}
	if err := provider.Init(); err != nil {
		t.Fatal(err)
	}
	defer provider.Destroy()

	const state = "github-single-use-state"
	if !provider.stateStore.Issue(state) {
		t.Fatal("failed to issue state")
	}
	res, err := provider.OnCallback(nil, state, map[string]string{"code": "auth-code"}, "")
	if err != nil {
		t.Fatalf("first OnCallback failed: %v", err)
	}
	if res.UserId != "42" {
		t.Fatalf("UserId = %q, want 42", res.UserId)
	}
	if _, err := provider.OnCallback(nil, state, map[string]string{"code": "auth-code"}, ""); err == nil || !strings.Contains(err.Error(), "invalid state") {
		t.Fatalf("replayed OnCallback error = %v, want invalid state", err)
	}

	// Oversized upstream token response must fail bounded read rather than buffering unbounded memory.
	provider.tokenURL = srv.URL + "/oversized"
	const bigState = "github-big-state"
	if !provider.stateStore.Issue(bigState) {
		t.Fatal("failed to issue bigState")
	}
	if _, err := provider.OnCallback(nil, bigState, map[string]string{"code": "auth-code"}, ""); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("%d", factory.MaxResponseBodyBytes)) {
		t.Fatalf("oversized response error = %v, want size cap error", err)
	}
}
