package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/komari-monitor/komari/web/connection"
)

type spyWSInterceptor struct {
	connectCalls atomic.Int32
	deny         bool
}

func (s *spyWSInterceptor) OnConnect(*connection.ConnInfo) (bool, string) {
	s.connectCalls.Add(1)
	if s.deny {
		return true, "blocked"
	}
	return false, ""
}

func (s *spyWSInterceptor) OnMessage(_ *connection.ConnInfo, frameType int, data []byte) (int, []byte, bool) {
	return frameType, data, false
}

func (s *spyWSInterceptor) OnSend(_ *connection.ConnInfo, frameType int, data []byte) (int, []byte, bool) {
	return frameType, data, false
}

func (s *spyWSInterceptor) OnClose(*connection.ConnInfo) {}

func TestPanelListenerWebSocketSkipsPluginInterceptor(t *testing.T) {
	prev := connection.Interceptor()
	spy := &spyWSInterceptor{deny: true}
	connection.SetFrameInterceptor(spy)
	t.Cleanup(func() { connection.SetFrameInterceptor(prev) })

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/panel/ws", func(c *gin.Context) {
		c.Set("panel_listener", true)
		conn, err := UpgradeSafeConn(c, RequireSameOriginWebSocket)
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.WriteMessage(websocket.TextMessage, []byte("panel-ok"))
	})
	r.GET("/public/ws", func(c *gin.Context) {
		conn, err := UpgradeSafeConn(c, RequireSameOriginWebSocket)
		if err != nil {
			return
		}
		_ = conn.Close()
	})

	server := httptest.NewServer(r)
	defer server.Close()
	header := http.Header{"Origin": {server.URL}}

	panelConn, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/panel/ws", header)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("panel websocket unexpectedly blocked: %v", err)
	}
	_, msg, err := panelConn.ReadMessage()
	_ = panelConn.Close()
	if err != nil || string(msg) != "panel-ok" {
		t.Fatalf("panel websocket message = %q (err=%v)", msg, err)
	}
	if calls := spy.connectCalls.Load(); calls != 0 {
		t.Fatalf("panel websocket invoked plugin interceptor %d times, want 0", calls)
	}

	pubConn, pubResp, pubErr := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/public/ws", header)
	if pubResp != nil {
		_ = pubResp.Body.Close()
	}
	if pubConn != nil {
		_, _, _ = pubConn.ReadMessage()
		_ = pubConn.Close()
	}
	if pubErr == nil && pubConn == nil {
		t.Fatal("expected public websocket to hit interceptor")
	}
	if calls := spy.connectCalls.Load(); calls != 1 {
		t.Fatalf("public websocket invoked plugin interceptor %d times, want 1", calls)
	}
}
