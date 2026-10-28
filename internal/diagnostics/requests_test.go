package diagnostics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	credentialv1 "github.com/r11234567/komari-proto/gen/go/komari/credential/v1"
	"google.golang.org/protobuf/types/known/emptypb"
)

func testEngine(r *Registry) *gin.Engine {
	gin.SetMode(gin.TestMode)
	e := gin.New()
	e.Use(r.Middleware())
	return e
}
func requestHTTP(t *testing.T, h http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(method, path, nil))
	return w
}
func find(t *testing.T, r *Registry, route string) RouteSnapshot {
	t.Helper()
	for _, v := range r.Snapshot().Routes {
		if v.Route == route {
			return v
		}
	}
	t.Fatalf("missing route %q: %+v", route, r.Snapshot())
	return RouteSnapshot{}
}

func TestDiagnosticsMeasuredSlowHTTPAndPrivacy(t *testing.T) {
	r := New()
	now := time.Unix(1, 0)
	r.clock = func() time.Time { return now }
	e := testEngine(r)
	e.POST("/files/:id", func(c *gin.Context) { now = now.Add(2 * time.Second); c.String(201, "unchanged-body") })
	req := httptest.NewRequest("POST", "/files/synthetic-private-name?token=synthetic-secret", strings.NewReader("synthetic-proof"))
	req.Header.Set("Authorization", "Bearer synthetic-bearer")
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)
	if w.Code != 201 || w.Body.String() != "unchanged-body" {
		t.Fatalf("changed response: %d %q", w.Code, w.Body.String())
	}
	v := find(t, r, "POST /files/:id")
	if v.DurationNS != int64(2*time.Second) || v.FirstResponseNS != int64(2*time.Second) || v.FirstResponseMaxNS != int64(2*time.Second) || v.FirstResponseCount != 1 || v.FirstResponseBuckets[5] != 1 || v.DurationBuckets[5] != 1 || v.HTTPStatusClasses[2] != 1 {
		t.Fatalf("incorrect measured latency: %+v", v)
	}
	raw, _ := json.Marshal(r.Snapshot())
	for _, secret := range []string{"synthetic-private-name", "synthetic-secret", "synthetic-bearer", "synthetic-proof"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("diagnostics leaked %s", secret)
		}
	}
	if len(r.Snapshot().RecentSlowOrFailed) != 1 {
		t.Fatal("slow operation missing")
	}
}

func TestDiagnosticsInFlightAndClientCancellation(t *testing.T) {
	r := New()
	e := testEngine(r)
	entered, exited := make(chan struct{}), make(chan struct{})
	e.GET("/wait", func(c *gin.Context) { close(entered); <-c.Request.Context().Done(); close(exited) })
	server := httptest.NewServer(e)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", server.URL+"/wait", nil)
	done := make(chan error, 1)
	go func() {
		resp, err := server.Client().Do(req)
		if resp != nil {
			resp.Body.Close()
		}
		done <- err
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("handler not entered")
	}
	if v := find(t, r, "GET /wait"); v.InFlight != 1 || v.Completed != 0 {
		t.Fatalf("live request hidden: %+v", v)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("client cancellation lost: %v", err)
	}
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("server did not observe cancel")
	}
	// A second handler synchronizes with completion without retry sleeps.
	server.Close()
	v := find(t, r, "GET /wait")
	if v.InFlight != 0 || v.Completed != 1 || v.Canceled != 1 {
		t.Fatalf("cancellation counters: %+v", v)
	}
}

func TestDiagnosticsConnectStreamErrorAndFlush(t *testing.T) {
	r := New()
	e := testEngine(r)
	const path = "/diagnostic.synthetic.Service/Watch"
	r.RegisterProcedure(path, true)
	handler := connect.NewServerStreamHandler(path, func(ctx context.Context, req *connect.Request[emptypb.Empty], stream *connect.ServerStream[credentialv1.CancelVerificationResponse]) error {
		if err := stream.Send(&credentialv1.CancelVerificationResponse{}); err != nil {
			return err
		}
		return connect.NewError(connect.CodeResourceExhausted, errors.New("synthetic-only"))
	}, connect.WithInterceptors(OutcomeInterceptor{}))
	e.POST(path, gin.WrapH(handler))
	server := httptest.NewServer(e)
	defer server.Close()
	client := connect.NewClient[emptypb.Empty, credentialv1.CancelVerificationResponse](server.Client(), server.URL+path)
	stream, err := client.CallServerStream(context.Background(), connect.NewRequest(&emptypb.Empty{}))
	if err != nil {
		t.Fatal(err)
	}
	if !stream.Receive() {
		t.Fatalf("stream payload buffered or missing: %v", stream.Err())
	}
	for stream.Receive() {
	}
	if connect.CodeOf(stream.Err()) != connect.CodeResourceExhausted {
		t.Fatalf("lost end-stream error: %v", stream.Err())
	}
	stream.Close()
	server.Close()
	v := find(t, r, "CONNECT "+path+" stream")
	if v.HTTPStatusClasses[2] != 1 || v.RPCCodes["resource_exhausted"] != 1 || v.FirstResponseCount != 1 || v.InFlight != 0 {
		t.Fatalf("HTTP200 stream error misclassified: %+v", v)
	}
}

func TestDiagnosticsLiveStreamDoesNotBuffer(t *testing.T) {
	r := New()
	e := testEngine(r)
	release := make(chan struct{})
	entered := make(chan struct{})
	e.GET("/stream", func(c *gin.Context) {
		c.Writer.WriteString("first\n")
		c.Writer.Flush()
		close(entered)
		<-release
		c.Writer.WriteString("last\n")
	})
	server := httptest.NewServer(e)
	defer server.Close()
	defer close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", server.URL+"/stream", nil)
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	<-entered
	first := make([]byte, 6)
	if _, err = io.ReadFull(resp.Body, first); err != nil || string(first) != "first\n" {
		t.Fatalf("early flush lost: %q %v", first, err)
	}
	v := find(t, r, "GET /stream")
	if v.InFlight != 1 || v.FirstResponseCount != 1 {
		t.Fatalf("stream not visible while open: %+v", v)
	}
}

func TestDiagnosticsPreservesWebSocketHijacking(t *testing.T) {
	r := New()
	e := testEngine(r)
	e.GET("/socket", func(c *gin.Context) {
		conn, err := (&websocket.Upgrader{}).Upgrade(c.Writer, c.Request, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		kind, body, err := conn.ReadMessage()
		if err == nil {
			conn.WriteMessage(kind, body)
		}
	})
	server := httptest.NewServer(e)
	defer server.Close()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/socket", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if err = conn.WriteMessage(websocket.TextMessage, []byte("synthetic-echo")); err != nil {
		t.Fatal(err)
	}
	_, body, err := conn.ReadMessage()
	if err != nil || string(body) != "synthetic-echo" {
		t.Fatalf("hijacked connection broken: %q %v", body, err)
	}
}

func TestDiagnosticsServerDeadlineIsNotClientCancellation(t *testing.T) {
	r := New()
	e := testEngine(r)
	const path = "/diagnostic.synthetic.Service/Deadline"
	r.RegisterProcedure(path, false)
	handler := connect.NewUnaryHandler(path, func(ctx context.Context, req *connect.Request[emptypb.Empty]) (*connect.Response[emptypb.Empty], error) {
		bounded, cancel := context.WithTimeout(ctx, 25*time.Millisecond)
		defer cancel()
		<-bounded.Done()
		return nil, connect.NewError(connect.CodeDeadlineExceeded, bounded.Err())
	}, connect.WithInterceptors(OutcomeInterceptor{}))
	e.POST(path, gin.WrapH(handler))
	server := httptest.NewServer(e)
	defer server.Close()
	client := connect.NewClient[emptypb.Empty, emptypb.Empty](server.Client(), server.URL+path)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := client.CallUnary(ctx, connect.NewRequest(&emptypb.Empty{}))
	if connect.CodeOf(err) != connect.CodeDeadlineExceeded {
		t.Fatalf("deadline outcome changed: %v", err)
	}
	server.Close()
	v := find(t, r, "CONNECT "+path+" unary")
	if v.RPCCodes["deadline_exceeded"] != 1 || v.Canceled != 0 || v.InFlight != 0 {
		t.Fatalf("server deadline misclassified: %+v", v)
	}
}

func TestDiagnosticsUnknownURLsAndRecentHistoryAreBounded(t *testing.T) {
	r := New()
	e := testEngine(r)
	for i := 0; i < 5000; i++ {
		requestHTTP(t, e, "GET", fmt.Sprintf("/nonexistent/%d?token=%d", i, i))
	}
	if len(r.Snapshot().Routes) != 1 {
		t.Fatal("raw URL cardinality allocated counters")
	}
	for i := 0; i < 2*MaxRoutes; i++ {
		c := r.get(fmt.Sprintf("GET /fixed-%d", i))
		c.started.Add(1)
		c.active.Add(1)
		(&request{registry: r, count: c, start: time.Now(), route: "GET /synthetic-error"}).finish(503, false, false)
	}
	s := r.Snapshot()
	if len(s.Routes) > MaxRoutes || len(s.RecentSlowOrFailed) != 64 {
		t.Fatalf("unbounded diagnostics: routes=%d events=%d", len(s.Routes), len(s.RecentSlowOrFailed))
	}
}

func TestDiagnosticsConcurrentSnapshotsAndAccounting(t *testing.T) {
	r := New()
	e := testEngine(r)
	e.GET("/ok", func(c *gin.Context) { c.Status(204) })
	var wg sync.WaitGroup
	var stop atomic.Bool
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for !stop.Load() {
			r.Snapshot()
		}
	}()
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				w := httptest.NewRecorder()
				e.ServeHTTP(w, httptest.NewRequest("GET", "/ok", nil))
			}
		}()
	}
	wg.Wait()
	stop.Store(true)
	<-readerDone
	v := find(t, r, "GET /ok")
	if v.Started != 1200 || v.Completed != 1200 || v.InFlight != 0 {
		t.Fatalf("lost concurrent observations: %+v", v)
	}
}

func BenchmarkDiagnosticsHTTP(b *testing.B) {
	for _, enabled := range []bool{false, true} {
		b.Run(fmt.Sprint(enabled), func(b *testing.B) {
			gin.SetMode(gin.TestMode)
			e := gin.New()
			if enabled {
				e.Use(New().Middleware())
			}
			e.GET("/ok", func(c *gin.Context) { c.Status(204) })
			req := httptest.NewRequest("GET", "/ok", nil)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				e.ServeHTTP(httptest.NewRecorder(), req)
			}
		})
	}
}
