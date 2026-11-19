package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

func TestTerminalWebSocketHandshakeRejectsCrossSiteOrigin(t *testing.T) {
	router := gin.New()
	router.GET("/terminal", func(c *gin.Context) {
		conn, err := UpgradeWebSocket(c, RequireSameOriginWebSocket)
		if err == nil {
			_ = conn.Close()
		}
	})
	server := httptest.NewServer(router)
	defer server.Close()
	for _, origin := range []string{"https://attacker.invalid", "null", "", server.URL} {
		header := http.Header{}
		if origin != "" {
			header.Set("Origin", origin)
		}
		conn, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/terminal", header)
		if conn != nil {
			_ = conn.Close()
		}
		if response != nil {
			_ = response.Body.Close()
		}
		if origin == server.URL {
			if err != nil {
				t.Fatalf("same-origin handshake rejected: %v", err)
			}
		} else if err == nil || response == nil || response.StatusCode != 403 {
			t.Fatalf("cross-site handshake accepted: origin=%q err=%v", origin, err)
		}
	}
}

func TestRemoteBrowserOriginSupportsInstalledMobileWebApps(t *testing.T) {
	tests := []struct {
		name   string
		origin string
		want   bool
	}{
		{name: "same origin", origin: "https://monitor.example", want: true},
		{name: "opaque mobile web app origin", origin: "null", want: true},
		{name: "missing mobile web app origin", origin: "", want: true},
		{name: "cross origin", origin: "https://attacker.example", want: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest("GET", "https://monitor.example/api/admin/client/remote", nil)
			request.Host = "monitor.example"
			if test.origin != "" {
				request.Header.Set("Origin", test.origin)
			}
			upgrader := &websocket.Upgrader{}
			RequireRemoteBrowserOrigin(upgrader)
			if got := upgrader.CheckOrigin(request); got != test.want {
				t.Fatalf("origin %q accepted=%v, want %v", test.origin, got, test.want)
			}
		})
	}
}
