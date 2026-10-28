// Package diagnostics collects bounded, process-local operational counters.
// It never stores request bodies, query strings, headers, principals or SQL.
package diagnostics

import (
	"context"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"
	"github.com/gin-gonic/gin"
)

const MaxRoutes = 512

var durationBounds = [...]time.Duration{10 * time.Millisecond, 50 * time.Millisecond, 100 * time.Millisecond, 500 * time.Millisecond, time.Second, 5 * time.Second, 15 * time.Second, 30 * time.Second}

type counter struct {
	started, completed, active, peak, canceled, panics atomic.Int64
	totalNS, maxNS, firstNS, firstCount, firstMaxNS    atomic.Int64
	buckets                                            [len(durationBounds) + 1]atomic.Int64
	firstBuckets                                       [len(durationBounds) + 1]atomic.Int64
	status                                             [6]atomic.Int64
	codes                                              [17]atomic.Int64
}

type procedure struct{ streaming bool }

type Registry struct {
	mu         sync.RWMutex
	startedAt  time.Time
	routes     map[string]*counter
	procedures map[string]procedure
	clock      func() time.Time
	recentMu   sync.Mutex
	recent     [64]SlowRequest
	recentNext uint64
}

var Default = New()

func New() *Registry {
	return &Registry{startedAt: time.Now().UTC(), routes: make(map[string]*counter), procedures: make(map[string]procedure), clock: time.Now}
}

// RegisterProcedure is called only from generated service descriptors at startup.
// An arbitrary URL must never allocate a procedure label.
func (r *Registry) RegisterProcedure(path string, streaming bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.procedures) < MaxRoutes {
		r.procedures[path] = procedure{streaming: streaming}
	}
}

func (r *Registry) route(method, path, pattern string) string {
	r.mu.RLock()
	p, ok := r.procedures[path]
	r.mu.RUnlock()
	if ok {
		kind := "unary"
		if p.streaming {
			kind = "stream"
		}
		return "CONNECT " + path + " " + kind
	}
	// FullPath is the server's router template, never Request.URL.Path.
	if pattern == "" {
		pattern = "/unmatched"
	}
	if strings.HasPrefix(pattern, "/komari.") {
		pattern = "/unknown-connect-procedure"
	}
	switch method {
	case "GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS":
	default:
		method = "OTHER"
	}
	return method + " " + pattern
}

func (r *Registry) get(route string) *counter {
	r.mu.RLock()
	c := r.routes[route]
	r.mu.RUnlock()
	if c != nil {
		return c
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if c = r.routes[route]; c != nil {
		return c
	}
	if len(r.routes) >= MaxRoutes-1 {
		route = "OTHER /overflow"
	}
	if c = r.routes[route]; c == nil {
		c = new(counter)
		r.routes[route] = c
	}
	return c
}

func raise(value *atomic.Int64, n int64) {
	for old := value.Load(); n > old; old = value.Load() {
		if value.CompareAndSwap(old, n) {
			return
		}
	}
}

type requestKey struct{}
type request struct {
	registry *Registry
	count    *counter
	start    time.Time
	first    atomic.Bool
	rpcCode  atomic.Int32
	panicked atomic.Bool
	route    string
}

func (q *request) firstResponse() {
	if q.first.CompareAndSwap(false, true) {
		d := max(time.Duration(0), q.registry.clock().Sub(q.start))
		q.count.firstNS.Add(int64(d))
		raise(&q.count.firstMaxNS, int64(d))
		i := 0
		for i < len(durationBounds) && d > durationBounds[i] {
			i++
		}
		q.count.firstBuckets[i].Add(1)
		q.count.firstCount.Add(1)
	}
}

func (q *request) finish(status int, canceled, panicked bool) {
	d := max(time.Duration(0), q.registry.clock().Sub(q.start))
	q.count.totalNS.Add(int64(d))
	raise(&q.count.maxNS, int64(d))
	i := 0
	for i < len(durationBounds) && d > durationBounds[i] {
		i++
	}
	q.count.buckets[i].Add(1)
	class := status / 100
	if class < 1 || class > 5 {
		class = 0
	}
	q.count.status[class].Add(1)
	code := q.rpcCode.Load()
	if code >= 0 && code < 17 {
		q.count.codes[code].Add(1)
	}
	if canceled {
		q.count.canceled.Add(1)
	}
	if panicked {
		q.count.panics.Add(1)
	}
	q.count.completed.Add(1)
	q.count.active.Add(-1)
	// Stream lifetime is not request latency. Record only failures for streams;
	// their first-response latency is reported separately above.
	if (!strings.HasSuffix(q.route, " stream") && d >= time.Second) || status >= 400 || canceled || panicked || code > 0 {
		q.registry.recentMu.Lock()
		q.registry.recent[q.registry.recentNext%uint64(len(q.registry.recent))] = SlowRequest{At: time.Now().UTC(), Route: q.route, DurationNS: int64(d), HTTPStatus: status, Canceled: canceled, RPCCode: code}
		q.registry.recentNext++
		q.registry.recentMu.Unlock()
	}
}

// MarkPanic is called by the existing recovery middleware; it does not recover,
// replace the response, or retain the panic value.
func MarkPanic(ctx context.Context) {
	if q, ok := ctx.Value(requestKey{}).(*request); ok {
		q.panicked.Store(true)
	}
}

type writer struct {
	gin.ResponseWriter
	observation *request
}

func (w *writer) WriteHeaderNow() { w.observation.firstResponse(); w.ResponseWriter.WriteHeaderNow() }
func (w *writer) Write(b []byte) (int, error) {
	w.observation.firstResponse()
	return w.ResponseWriter.Write(b)
}
func (w *writer) WriteString(s string) (int, error) {
	w.observation.firstResponse()
	return w.ResponseWriter.WriteString(s)
}
func (w *writer) Flush() { w.observation.firstResponse(); w.ResponseWriter.Flush() }

// Middleware must run before authentication and recovery, so a blocked identity
// lookup and recovered panic count towards the observed handler duration.
// It does not buffer or consume request/response bodies or start a goroutine.
func (r *Registry) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		route := r.route(c.Request.Method, c.Request.URL.Path, c.FullPath())
		count := r.get(route)
		q := &request{registry: r, count: count, start: r.clock(), route: route}
		q.rpcCode.Store(-1)
		count.started.Add(1)
		raise(&count.peak, count.active.Add(1))
		c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), requestKey{}, q))
		c.Writer = &writer{ResponseWriter: c.Writer, observation: q}
		finished := false
		defer func() { q.finish(c.Writer.Status(), c.Request.Context().Err() != nil, !finished || q.panicked.Load()) }()
		c.Next()
		// Gin sends an implicit response after returning through middleware.
		if !c.Writer.Written() {
			c.Writer.WriteHeaderNow()
		}
		finished = true
	}
}

// OutcomeInterceptor records the Connect outcome even when a stream returns
// HTTP 200 and the actual error is carried in its end-stream envelope.
type OutcomeInterceptor struct{}

func (OutcomeInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		resp, err := next(ctx, req)
		recordOutcome(ctx, err)
		return resp, err
	}
}
func (OutcomeInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}
func (OutcomeInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		err := next(ctx, conn)
		recordOutcome(ctx, err)
		return err
	}
}
func recordOutcome(ctx context.Context, err error) {
	if q, ok := ctx.Value(requestKey{}).(*request); ok {
		code := int32(0)
		if err != nil {
			code = int32(connect.CodeOf(err))
		}
		q.rpcCode.Store(code)
	}
}

type RouteSnapshot struct {
	Route                string           `json:"route"`
	Started              int64            `json:"started"`
	Completed            int64            `json:"completed"`
	InFlight             int64            `json:"in_flight"`
	PeakInFlight         int64            `json:"peak_in_flight"`
	Canceled             int64            `json:"client_canceled"`
	Panics               int64            `json:"panics"`
	DurationNS           int64            `json:"duration_sum_ns"`
	MaxDurationNS        int64            `json:"duration_max_ns"`
	FirstResponseNS      int64            `json:"first_response_sum_ns"`
	FirstResponseCount   int64            `json:"first_response_count"`
	FirstResponseMaxNS   int64            `json:"first_response_max_ns"`
	FirstResponseBuckets []int64          `json:"first_response_buckets"`
	DurationBuckets      []int64          `json:"duration_buckets"`
	HTTPStatusClasses    []int64          `json:"http_status_classes"`
	RPCCodes             map[string]int64 `json:"rpc_codes"`
}
type RequestSnapshot struct {
	StartedAt          time.Time       `json:"started_at"`
	CollectedAt        time.Time       `json:"collected_at"`
	DurationBoundsNS   []int64         `json:"duration_bounds_ns"`
	Routes             []RouteSnapshot `json:"routes"`
	RecentSlowOrFailed []SlowRequest   `json:"recent_slow_or_failed"`
}

type SlowRequest struct {
	At         time.Time `json:"at"`
	Route      string    `json:"route"`
	DurationNS int64     `json:"duration_ns"`
	HTTPStatus int       `json:"http_status"`
	Canceled   bool      `json:"client_canceled"`
	RPCCode    int32     `json:"rpc_code"`
}

// Snapshot samples atomic counters without stopping request handling. Values
// are cumulative, approximately simultaneous; compare two snapshots for rates.
func (r *Registry) Snapshot() RequestSnapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := RequestSnapshot{StartedAt: r.startedAt, CollectedAt: time.Now().UTC(), Routes: make([]RouteSnapshot, 0, len(r.routes))}
	for _, bound := range durationBounds {
		out.DurationBoundsNS = append(out.DurationBoundsNS, int64(bound))
	}
	for route, c := range r.routes {
		v := RouteSnapshot{Route: route, Started: c.started.Load(), Completed: c.completed.Load(), InFlight: c.active.Load(), PeakInFlight: c.peak.Load(), Canceled: c.canceled.Load(), Panics: c.panics.Load(), DurationNS: c.totalNS.Load(), MaxDurationNS: c.maxNS.Load(), FirstResponseNS: c.firstNS.Load(), FirstResponseCount: c.firstCount.Load(), RPCCodes: make(map[string]int64)}
		v.FirstResponseMaxNS = c.firstMaxNS.Load()
		for i := range c.buckets {
			v.DurationBuckets = append(v.DurationBuckets, c.buckets[i].Load())
			v.FirstResponseBuckets = append(v.FirstResponseBuckets, c.firstBuckets[i].Load())
		}
		for i := range c.status {
			v.HTTPStatusClasses = append(v.HTTPStatusClasses, c.status[i].Load())
		}
		for i := range c.codes {
			if n := c.codes[i].Load(); n > 0 {
				name := "ok"
				if i > 0 {
					name = connect.Code(i).String()
				}
				v.RPCCodes[name] = n
			}
		}
		out.Routes = append(out.Routes, v)
	}
	sort.Slice(out.Routes, func(i, j int) bool { return out.Routes[i].Route < out.Routes[j].Route })
	r.recentMu.Lock()
	start := uint64(0)
	if r.recentNext > uint64(len(r.recent)) {
		start = r.recentNext - uint64(len(r.recent))
	}
	for i := start; i < r.recentNext; i++ {
		out.RecentSlowOrFailed = append(out.RecentSlowOrFailed, r.recent[i%uint64(len(r.recent))])
	}
	r.recentMu.Unlock()
	return out
}
