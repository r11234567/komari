package generic

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/komari-monitor/komari/web/oauth/factory"
)

func TestGenericOnCallbackConsumesStateOnceAndRejectsOversizedResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"gen_test_token"}`))
		case "/user":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"sub":"user-99"}`))
		case "/oversized":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"`))
			_, _ = w.Write([]byte(strings.Repeat("y", int(factory.MaxResponseBodyBytes)+64)))
			_, _ = w.Write([]byte(`"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	provider := &Generic{
		Addition: Addition{
			ClientId:     "cid",
			ClientSecret: "csec",
			AuthURL:      srv.URL + "/auth",
			TokenURL:     srv.URL + "/token",
			UserInfoURL:  srv.URL + "/user",
			UserIDField:  "sub",
		},
	}
	if err := provider.Init(); err != nil {
		t.Fatal(err)
	}
	defer provider.Destroy()

	const state = "generic-single-use-state"
	if !provider.stateStore.Issue(state) {
		t.Fatal("failed to issue state")
	}
	res, err := provider.OnCallback(nil, state, map[string]string{"code": "auth-code"}, "https://panel.example/api/oauth_callback")
	if err != nil {
		t.Fatalf("first OnCallback failed: %v", err)
	}
	if res.UserId != "user-99" {
		t.Fatalf("UserId = %q, want user-99", res.UserId)
	}
	if _, err := provider.OnCallback(nil, state, map[string]string{"code": "auth-code"}, "https://panel.example/api/oauth_callback"); err == nil || !strings.Contains(err.Error(), "invalid state") {
		t.Fatalf("replayed OnCallback error = %v, want invalid state", err)
	}

	provider.Addition.TokenURL = srv.URL + "/oversized"
	const bigState = "generic-big-state"
	if !provider.stateStore.Issue(bigState) {
		t.Fatal("failed to issue bigState")
	}
	if _, err := provider.OnCallback(nil, bigState, map[string]string{"code": "auth-code"}, "https://panel.example/api/oauth_callback"); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("%d", factory.MaxResponseBodyBytes)) {
		t.Fatalf("oversized response error = %v, want size cap error", err)
	}
}
