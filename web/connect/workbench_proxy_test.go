package connectapi

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/komari-monitor/komari/pkg/rpc"
	"github.com/komari-monitor/komari/web/filemanager"
	"github.com/komari-monitor/komari/web/workspace"
	filev1 "github.com/r11234567/komari-proto/gen/go/komari/file/v1"
	filev1connect "github.com/r11234567/komari-proto/gen/go/komari/file/v1/filev1connect"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

// This is a transport component checkpoint, not a browser/full deployment E2E.
// Actual Nginx must deliver a small lease before EOF, then support the file
// attach request/response handshake while the Agent request body remains open.
func TestWorkbenchNginxFileLeaseAndDuplexAttach(t *testing.T) {
	if os.Getenv("KOMARI_NGINX_COMPONENT_TEST") != "1" {
		t.Skip("runs only on the disposable GitHub runner with Nginx installed")
	}
	nginx, err := exec.LookPath("nginx")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	store := workspace.NewStore(time.Now, func(workspace.Binding) bool { return true })
	previous := workspace.Default
	workspace.Default = store
	defer func() { workspace.Default = previous }()
	token, snapshot, err := store.Create(workspace.Binding{UserID: "fixture-admin", Session: "fixture-session", AgentID: "fixture-agent", SystemUser: "fixture-user"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Lock(token, "fixture-admin", "fixture-session")
	service := newFileService()
	defer filemanager.SetDispatcher(nil)
	_, handler := filev1connect.NewFileServiceHandler(service, connect.WithInterceptors(newPolicyInterceptor()))
	upstream := httptest.NewServer(h2c.NewHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		meta := &rpc.ContextMeta{Principal: rpc.NewAgentPrincipal("fixture-agent"), ClientUUID: "fixture-agent"}
		handler.ServeHTTP(w, r.WithContext(rpc.NewContextWithMeta(r.Context(), meta)))
	}), &http2.Server{}))
	defer upstream.Close()
	endpoint := startWorkbenchNginx(t, nginx, strings.TrimPrefix(upstream.URL, "http://"))
	client := filev1connect.NewFileServiceClient(workbenchHTTP2Client(t), endpoint)
	leaseResult := make(chan *filev1.FileAssignment, 1)
	leaseError := make(chan error, 1)
	go func() {
		req := connect.NewRequest(&filev1.LeaseFileOperationsRequest{AgentId: "fixture-agent"})
		req.Header().Set(workspace.IdentityHeader, "v1")
		stream, err := client.LeaseFileOperations(ctx, req)
		if err != nil {
			leaseError <- err
			return
		}
		defer stream.Close()
		if !stream.Receive() {
			leaseError <- fmt.Errorf("receive file lease: %v", stream.Err())
			return
		}
		if stream.ResponseHeader().Get("X-Accel-Buffering") != "" {
			// Nginx normally consumes X-Accel-Buffering instead of forwarding it.
			t.Logf("stream header: %s", stream.ResponseHeader().Get("X-Accel-Buffering"))
		}
		leaseResult <- stream.Msg().Assignment
	}()
	// Wait for registration rather than racing the first dispatch against the
	// lease request. Registration happens before the first response is sent.
	for {
		service.store.mu.Lock()
		registered := service.store.agents["fixture-agent"] != nil
		service.store.mu.Unlock()
		if registered {
			break
		}
		select {
		case err := <-leaseError:
			t.Fatal(err)
		case <-ctx.Done():
			t.Fatal("file lease never registered through Nginx")
		case <-time.After(10 * time.Millisecond):
		}
	}
	bound, stop := workspace.Bind(ctx, snapshot.Grant)
	defer stop()
	result := make(chan error, 1)
	go func() {
		event, err := filemanager.Call(bound, "fixture-agent", "list", map[string]any{"path": "/home/fixture-user"})
		if err == nil && (event.Path != "/home/fixture-user" || !event.Success) {
			err = fmt.Errorf("unexpected directory response: %v", event)
		}
		result <- err
	}()
	var assignment *filev1.FileAssignment
	select {
	case assignment = <-leaseResult:
	case err := <-leaseError:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal("small file assignment was buffered instead of delivered")
	}
	stream := client.AttachFileOperations(ctx)
	stream.RequestHeader().Set(workspace.IdentityHeader, "v1")
	defer stream.CloseResponse()
	if err := stream.Send(&filev1.AttachFileOperationsRequest{Message: &filev1.AttachFileOperationsRequest_Attach{Attach: &filev1.AgentFileAttach{
		AgentId: "fixture-agent", AssignmentId: assignment.AssignmentId, RequestId: assignment.RequestId,
	}}}); err != nil {
		t.Fatal(err)
	}
	command, err := stream.Receive()
	if err != nil {
		t.Fatalf("file duplex handshake before request EOF: %v", err)
	}
	if command.GetAssignment().GetSystemUser() != "fixture-user" || command.GetAssignment().GetWorkspaceId() != snapshot.Grant.ID {
		t.Fatal("proxy lost selected-account workspace binding")
	}
	// Inherit a deliberately short 1s global body timeout from the fixture.
	// The workbench location must override it during an idle duplex request.
	time.Sleep(1200 * time.Millisecond)
	if err := stream.Send(&filev1.AttachFileOperationsRequest{Message: &filev1.AttachFileOperationsRequest_Event{Event: &filev1.AgentFileEvent{
		RequestId: assignment.RequestId, Operation: assignment.Operation, Success: true, Path: "/home/fixture-user",
	}}}); err != nil {
		t.Fatal(err)
	}
	_ = stream.CloseRequest()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("file metadata response did not reach the caller")
	}
	if _, err := stream.Receive(); !errors.Is(err, io.EOF) {
		t.Fatalf("attach completion: %v", err)
	}
	t.Log("small file lease and account-bound duplex result completed through checked-in Nginx routes")
}

func workbenchHTTP2Client(t *testing.T) *http.Client {
	t.Helper()
	transport := &http2.Transport{AllowHTTP: true, DialTLSContext: func(ctx context.Context, network, address string, _ *tls.Config) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}}
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{Transport: transport}
}

func startWorkbenchNginx(t *testing.T, nginx, upstream string, defaults ...string) string {
	t.Helper()
	directory := t.TempDir()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	locations, err := filepath.Abs("../../deploy/nginx/workbench-locations.conf")
	if err != nil {
		t.Fatal(err)
	}
	config := fmt.Sprintf(`pid %s/nginx.pid;
error_log stderr info;
events { worker_connections 64; }
http {
  client_body_timeout 1s;
  %s
  access_log %s/access.log;
  upstream komari_backend { server %s; }
  server {
    listen %s http2;
    include %s;
    location / { proxy_pass http://komari_backend; proxy_http_version 1.1; }
  }
}
`, directory, strings.Join(defaults, "\n"), directory, upstream, address, locations)
	configPath := filepath.Join(directory, "nginx.conf")
	if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	log, err := os.Create(filepath.Join(directory, "nginx.log"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(nginx, "-p", directory+"/", "-c", configPath, "-g", "daemon off; master_process off;")
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		log.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = log.Close()
		for _, name := range []string{"nginx.conf", "nginx.log", "access.log"} {
			data, err := os.ReadFile(filepath.Join(directory, name))
			if err == nil {
				t.Logf("%s:\n%s", name, data)
			}
		}
	})
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", address, 100*time.Millisecond)
		if err == nil {
			conn.Close()
			return "http://" + address
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("Nginx did not start within 3s; see retained nginx.log")
	return ""
}
