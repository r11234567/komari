package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

func TestPublicClientSocketRequestsAndFrameLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/api/clients", GetClients)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/api/clients", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for _, query := range []string{"get", "get 24c1a88f-0b50-4fd1-9864-032648e9f92f"} {
		if err := conn.WriteMessage(websocket.TextMessage, []byte(query)); err != nil {
			t.Fatal(err)
		}
		_, message, err := conn.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		var response struct{ Status string }
		if err := json.Unmarshal(message, &response); err != nil || response.Status != "success" {
			t.Fatalf("public query %q returned %s: %v", query, message, err)
		}
	}
	if err := conn.WriteMessage(websocket.TextMessage, []byte(strings.Repeat("a", 257))); err != nil {
		t.Fatal(err)
	}
	if _, _, err := conn.ReadMessage(); !websocket.IsCloseError(err, websocket.CloseMessageTooBig) {
		t.Fatalf("expected oversized frame to close the socket, got %v", err)
	}
}

func TestPublicClientSocketRejectsConnectionsAtCapacity(t *testing.T) {
	for range cap(publicClientConnections) {
		publicClientConnections <- struct{}{}
	}
	t.Cleanup(func() {
		for range cap(publicClientConnections) {
			<-publicClientConnections
		}
	})
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/api/clients", GetClients)
	request := httptest.NewRequest(http.MethodGet, "/api/clients", nil)
	request.Header.Set("Upgrade", "websocket")
	request.Header.Set("Connection", "Upgrade")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("over capacity returned HTTP %d", response.Code)
	}
}
