package security

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/pkg/rpc"
)

func TestPrincipalClassCoversEveryCredentialKind(t *testing.T) {
	tests := []struct {
		name      string
		principal *rpc.Principal
		want      string
	}{
		{"agent", rpc.NewAgentPrincipal("node-a"), PrincipalHeaderAgent},
		{"user", rpc.NewUserPrincipal("user-1"), PrincipalHeaderUser},
		{"api key", rpc.NewAPIKeyPrincipal(), PrincipalHeaderAPIKey},
		{"anonymous", rpc.NewAnonymousPrincipal(), PrincipalHeaderAnonymous},
		{"nil", nil, PrincipalHeaderAnonymous},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := PrincipalClass(test.principal); got != test.want {
				t.Fatalf("PrincipalClass = %q, want %q", got, test.want)
			}
		})
	}
}

func TestPrincipalHeaderCarriesNoAccountIdentity(t *testing.T) {
	// The header is read from proxy access logs. It must classify the
	// credential without naming the account or agent behind it.
	for _, principal := range []*rpc.Principal{
		rpc.NewAgentPrincipal("node-a-secret-uuid"),
		rpc.NewUserPrincipal("user-secret-uuid"),
	} {
		value := PrincipalClass(principal)
		if value == principal.ClientUUID || value == principal.UserUUID {
			t.Fatalf("header value %q leaked an identity", value)
		}
	}
}

func TestPrincipalHeaderIsPresentOnRejectedResponses(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		SetPrincipalHeader(c, rpc.NewAnonymousPrincipal())
		c.Next()
	})
	// The banning layer counts error responses, so the classification has to
	// survive an abort - setting it only on success would leave exactly the
	// interesting responses unlabelled.
	router.GET("/api/denied", func(c *gin.Context) {
		c.AbortWithStatus(http.StatusUnauthorized)
	})

	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/denied", nil))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", response.Code)
	}
	if got := response.Header().Get(PrincipalHeader); got != PrincipalHeaderAnonymous {
		t.Fatalf("%s on a 401 = %q, want %q", PrincipalHeader, got, PrincipalHeaderAnonymous)
	}
}

func TestPrincipalHeaderMarksAuthenticatedTraffic(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		SetPrincipalHeader(c, rpc.NewAgentPrincipal("node-a"))
		c.Next()
	})
	router.POST("/komari.report.v1.AgentReportService/SubmitReport", func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/komari.report.v1.AgentReportService/SubmitReport", nil))
	if got := response.Header().Get(PrincipalHeader); got != PrincipalHeaderAgent {
		t.Fatalf("%s = %q, want %q", PrincipalHeader, got, PrincipalHeaderAgent)
	}
}
