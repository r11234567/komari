package plugin

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Calcium-Ion/moejs"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

func TestPluginRPCRejectsSystemNamespaces(t *testing.T) {
	manager := &Manager{}
	for _, method := range []string{"rpc.ping", "admin:newMethod", "client:report", "public:fake", "common:fake", "ADMIN:fake"} {
		if err := manager.registerRPC("fixture", method, moejs.Undefined()); err == nil {
			t.Fatalf("reserved RPC namespace accepted: %s", method)
		}
	}
}

func TestWorkspaceCredentialsNeverEnterGenericPluginHooks(t *testing.T) {
	withTempDataDir(t)
	gin.SetMode(gin.TestMode)
	Init(gin.New())
	zipPath := writePluginZip(t, map[string]string{
		"komari-plugin.json": `{"name":"Boundary","short":"boundary","version":"1.0.0","permissions":{"allowHooks":true}}`,
		"script.js":          `const server=require("server");function load(){server.hook("request",()=>{throw new Error("unexpected credential exposure")});}`,
	})
	if _, err := InstallZip(zipPath); err != nil {
		t.Fatal(err)
	}
	if err := SetEnabled("boundary", true, true); err != nil {
		t.Fatal(err)
	}
	defer SetEnabled("boundary", false, false)
	wrapped := WrapHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.WriteHeader(207)
		_, _ = w.Write(body)
	}))
	for _, path := range []string{"/komari.workspace.v1.WorkspaceService/UnlockWorkspace", "/komari.webssh.v1.WebSSHService/CreateSession", "/komari.file.v1.FileService/ListDirectory", "/api/admin/client/agent/file/upload", "/api/preview/client/agent/file/download", "/api/clients/transfer/opaque"} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest("POST", path, strings.NewReader("sensitive-fixture"))
		request.Header.Set("X-Komari-Workspace-Grant", "synthetic-token")
		wrapped.ServeHTTP(recorder, request)
		if recorder.Code != 207 || recorder.Body.String() != "sensitive-fixture" {
			t.Fatalf("workspace passed through plugin hooks: %s status=%d body=%s", path, recorder.Code, recorder.Body.String())
		}
	}
	recorder := httptest.NewRecorder()
	wrapped.ServeHTTP(recorder, httptest.NewRequest("POST", "/custom-plugin-path", strings.NewReader("fixture")))
	if recorder.Code != 500 {
		t.Fatal("fixture hook was not active on its permitted surface")
	}
}

func TestMoeJSOwnedWebSocketEchoAndUnload(t *testing.T) {
	withTempDataDir(t)
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	Init(engine)
	zipPath := writePluginZip(t, map[string]string{
		"komari-plugin.json": `{"name":"WebSocket","short":"moews","version":"1.0.0","permissions":{"allowRoutes":true,"timeout":3}}`,
		"script.js":          `const server=require("server");function load(){server.route("GET","/moe-ws",async(req,res)=>{const ws=await req.upgradeWebSocket();ws.onmessage=(event)=>ws.send(event.data);ws.send("ready")})}`,
	})
	if _, err := InstallZip(zipPath); err != nil {
		t.Fatal(err)
	}
	if err := SetEnabled("moews", true, true); err != nil {
		t.Fatal(err)
	}
	defer SetEnabled("moews", false, false)
	server := httptest.NewServer(WrapHandler(engine))
	defer server.Close()
	conn, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/moe-ws", http.Header{"Origin": []string{server.URL}})
	if err != nil {
		if response != nil {
			defer response.Body.Close()
			body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
			t.Fatalf("WebSocket handshake: %v; status=%s body=%s plugin=%s", err, response.Status, body, GetLogs("moews"))
		}
		t.Fatalf("WebSocket handshake: %v; plugin=%s", err, GetLogs("moews"))
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, data, err := conn.ReadMessage()
	if err != nil || string(data) != "ready" {
		t.Fatalf("ws ready: %s %v", data, err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, []byte("hello MoeJS")); err != nil {
		t.Fatal(err)
	}
	_, data, err = conn.ReadMessage()
	if err != nil || string(data) != "hello MoeJS" {
		t.Fatalf("ws echo: %s %v", data, err)
	}
	if err := SetEnabled("moews", false, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := conn.ReadMessage(); err == nil {
		t.Fatal("plugin unload did not close its socket")
	}
}
