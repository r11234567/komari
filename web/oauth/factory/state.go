package factory

import (
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

const (
	// DefaultStateTTL bounds how long an issued OAuth state token remains valid.
	DefaultStateTTL = 10 * time.Minute
	// DefaultMaxStates bounds the number of in-flight OAuth states held in memory.
	DefaultMaxStates = 4096
	// DefaultHTTPTimeout bounds outbound OAuth provider HTTP requests.
	DefaultHTTPTimeout = 15 * time.Second
	// MaxResponseBodyBytes bounds OAuth provider response payloads against memory exhaustion.
	MaxResponseBodyBytes int64 = 1 << 20
)

// StateStore tracks short-lived, single-use OAuth state tokens in a bounded,
// concurrency-safe table.
type StateStore struct {
	mu         sync.Mutex
	ttl        time.Duration
	maxEntries int
	now        func() time.Time
	entries    map[string]time.Time
}

// NewStateStore constructs a bounded single-use state store.
func NewStateStore(ttl time.Duration, maxEntries int) *StateStore {
	if ttl <= 0 {
		ttl = DefaultStateTTL
	}
	if maxEntries <= 0 {
		maxEntries = DefaultMaxStates
	}
	return &StateStore{
		ttl:        ttl,
		maxEntries: maxEntries,
		now:        time.Now,
		entries:    make(map[string]time.Time),
	}
}

func (s *StateStore) pruneLocked(now time.Time) {
	for k, exp := range s.entries {
		if !now.Before(exp) {
			delete(s.entries, k)
		}
	}
}

// Issue registers a freshly generated OAuth state token if capacity permits.
func (s *StateStore) Issue(state string) bool {
	if s == nil || state == "" {
		return false
	}
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entries == nil {
		s.entries = make(map[string]time.Time)
	}
	s.pruneLocked(now)
	if len(s.entries) >= s.maxEntries {
		return false
	}
	s.entries[state] = now.Add(s.ttl)
	return true
}

// Consume atomically validates and removes an OAuth state token so concurrent or
// repeated callbacks can never replay the same state.
func (s *StateStore) Consume(state string) bool {
	if s == nil || state == "" {
		return false
	}
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entries == nil {
		return false
	}
	exp, ok := s.entries[state]
	delete(s.entries, state)
	if !ok {
		return false
	}
	return now.Before(exp)
}

// Reset clears all pending OAuth states.
func (s *StateStore) Reset() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	clear(s.entries)
}

// NewHTTPClient returns an HTTP client configured with a bounded timeout for OAuth calls.
func NewHTTPClient() *http.Client {
	return &http.Client{Timeout: DefaultHTTPTimeout}
}

// ReadBoundedBody reads at most maxBytes from r, failing if the payload exceeds the cap.
func ReadBoundedBody(r io.Reader, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		maxBytes = MaxResponseBodyBytes
	}
	data, err := io.ReadAll(io.LimitReader(r, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("oauth response body exceeds %d bytes", maxBytes)
	}
	return data, nil
}

// DoBoundedRequest executes req using client (or a default bounded client) and
// returns the HTTP status code and bounded response body.
func DoBoundedRequest(client *http.Client, req *http.Request, maxBytes int64) (int, []byte, error) {
	if client == nil {
		client = NewHTTPClient()
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := ReadBoundedBody(resp.Body, maxBytes)
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, body, nil
}
