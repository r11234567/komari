package terminal

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/web/connection"
)

// browserPlaceholder returns a non-nil browser connection. The rejection paths
// under test only check it against nil, so a zero value is never written to.
func browserPlaceholder() *connection.SafeConn {
	return &connection.SafeConn{}
}

// The legacy agent terminal route admits any enrolled agent, so a session must
// only be answerable by the machine it was opened against. Matching on the
// session id alone would let an agent that learns an id become the PTY backend
// for an administrator's shell on a different host.
func TestEstablishConnectionRejectsForeignAgents(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const sessionID = "session-under-test"
	cases := []struct {
		name   string
		caller string
	}{
		{"a different enrolled agent", "agent-b"},
		{"an unauthenticated caller", ""},
		{"the requesting administrator", "admin-1"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A non-nil Browser is required for the handler to look past the
			// existence check, so the ownership check is what must reject these.
			session := &TerminalSession{UUID: "agent-a", UserUUID: "admin-1", Browser: browserPlaceholder()}
			TerminalSessionsMutex.Lock()
			TerminalSessions[sessionID] = session
			TerminalSessionsMutex.Unlock()
			t.Cleanup(func() {
				TerminalSessionsMutex.Lock()
				delete(TerminalSessions, sessionID)
				TerminalSessionsMutex.Unlock()
			})

			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodGet, "/api/clients/terminal", nil)
			c.Request.Header.Set("X-Komari-Terminal-Session", sessionID)
			if tc.caller != "" {
				c.Set("client_uuid", tc.caller)
			}

			EstablishConnection(c)

			if recorder.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404: a foreign agent must not learn the session exists", recorder.Code)
			}
			// The session must be left intact for its rightful agent.
			TerminalSessionsMutex.Lock()
			stored, ok := TerminalSessions[sessionID]
			TerminalSessionsMutex.Unlock()
			if !ok || stored.Agent != nil {
				t.Fatal("a rejected caller must not claim or destroy the session")
			}
		})
	}
}

// An unknown session id is refused with the same status as a foreign agent, so
// the response cannot be used to enumerate live sessions.
func TestEstablishConnectionHidesUnknownSessions(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/clients/terminal", nil)
	c.Request.Header.Set("X-Komari-Terminal-Session", "no-such-session")
	c.Set("client_uuid", "agent-a")

	EstablishConnection(c)

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", recorder.Code)
	}
}
