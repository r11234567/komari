package security

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/gin-gonic/gin"
	"google.golang.org/protobuf/types/known/emptypb"
)

func TestRateLimitRejectionsPreserveRPCProtocol(t *testing.T) {
	for protocol, options := range map[string][]connect.ClientOption{
		"connect": nil, "grpc": {connect.WithGRPC()}, "grpc-web": {connect.WithGRPCWeb()},
	} {
		for _, streaming := range []bool{false, true} {
			name := "unary"
			if streaming {
				name = "server-stream"
			}
			t.Run(protocol+"/"+name, func(t *testing.T) {
				controller := NewRateLimitController(true)
				path := "/komari.test.v1.BudgetService/Read"
				key := "read:user:wire-test"
				if streaming {
					path = "/komari.test.v1.BudgetService/WatchSession"
					key = "stream:user:wire-test"
				}
				router := gin.New()
				router.Use(func(c *gin.Context) { c.Set("uuid", "wire-test"); c.Next() }, controller.Middleware())
				if streaming {
					handler := connect.NewServerStreamHandler(path, func(ctx context.Context, req *connect.Request[emptypb.Empty], stream *connect.ServerStream[emptypb.Empty]) error {
						return stream.Send(&emptypb.Empty{})
					})
					router.POST(path, gin.WrapH(handler))
				} else {
					handler := connect.NewUnaryHandler(path, func(context.Context, *connect.Request[emptypb.Empty]) (*connect.Response[emptypb.Empty], error) {
						return connect.NewResponse(&emptypb.Empty{}), nil
					})
					router.POST(path, gin.WrapH(handler))
				}
				server := httptest.NewUnstartedServer(router)
				server.EnableHTTP2 = true
				server.StartTLS()
				t.Cleanup(server.Close)
				client := connect.NewClient[emptypb.Empty, emptypb.Empty](server.Client(), server.URL+path, options...)
				call := func() error {
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					request := connect.NewRequest(&emptypb.Empty{})
					if !streaming {
						_, err := client.CallUnary(ctx, request)
						return err
					}
					stream, err := client.CallServerStream(ctx, request)
					if err != nil {
						return err
					}
					defer stream.Close()
					for stream.Receive() {
					}
					return stream.Err()
				}
				if err := call(); err != nil {
					t.Fatalf("request within budget: %v", err)
				}
				// Freeze a depleted bucket ahead of the test so transport scheduling
				// cannot refill it; token accounting is covered by the budget tests.
				controller.mu.Lock()
				controller.buckets[key] = rateLimitBucket{seen: time.Now().Add(time.Minute)}
				controller.mu.Unlock()
				err := call()
				var wireError *connect.Error
				if !errors.As(err, &wireError) || !connect.IsWireError(err) || wireError.Code() != connect.CodeResourceExhausted {
					t.Fatalf("limit must decode as a native resource_exhausted error: %v", err)
				}
				if wireError.Message() != "request rate limit exceeded" {
					t.Fatalf("native limit message lost: %q", wireError.Message())
				}
				if retry, parseErr := strconv.Atoi(wireError.Meta().Get("Retry-After")); parseErr != nil || retry < 1 {
					t.Fatalf("missing retry metadata: %v", wireError.Meta())
				}
				if wireError.Meta().Get("Cache-Control") != "no-store" {
					t.Fatalf("limit may be cached: %v", wireError.Meta())
				}
			})
		}
	}
}

func TestRateLimitRESTErrorShapeIsPreserved(t *testing.T) {
	controller := NewRateLimitController(true)
	controller.buckets["read:user:rest-test"] = rateLimitBucket{seen: time.Now().Add(time.Minute)}
	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set("uuid", "rest-test"); c.Next() }, controller.Middleware())
	router.GET("/api/test", func(c *gin.Context) { t.Error("depleted request reached handler") })
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/test", nil))
	if response.Code != http.StatusTooManyRequests || response.Body.String() != `{"code":"resource_exhausted","error":"request rate limit exceeded","message":"request rate limit exceeded"}` {
		t.Fatalf("REST limit contract changed: %d %s", response.Code, response.Body.String())
	}
}
