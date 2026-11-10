package security

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/pkg/config"
)

func TestRateLimitNativeLoginAndPrincipalIsolation(t *testing.T) {
	t.Setenv(TrustedProxyEnv, "none")
	controller := NewRateLimitController(true)
	contextFor := func(path, agent, token, forwarded string) *gin.Context {
		c, engine := gin.CreateTestContext(httptest.NewRecorder())
		if err := ConfigureTrustedProxies(engine, "none"); err != nil {
			t.Fatal(err)
		}
		c.Request = httptest.NewRequest(http.MethodPost, path, nil)
		c.Request.RemoteAddr = "192.0.2.10:1234"
		c.Request.Header.Set("X-Forwarded-For", forwarded)
		if agent != "" {
			c.Set("role", "client")
			c.Set("client_uuid", agent)
		}
		c.Set("api_token_id", token)
		return c
	}
	login := "/komari.credential.v1.PasswordLoginService/BeginPasswordLogin"
	first, rate, burst := controller.keyAndBudget(contextFor(login, "", "", "1.1.1.1"))
	second, _, _ := controller.keyAndBudget(contextFor(login, "", "", "8.8.8.8"))
	if first != second || rate != 0.2 || burst != 5 {
		t.Fatal("native login budget trusted a spoofed forwarding header or missed login classification")
	}
	path := "/komari.metrics.v1.MetricsService/QueryMetrics"
	a, _, _ := controller.keyAndBudget(contextFor(path, "", "token-a", ""))
	b, _, _ := controller.keyAndBudget(contextFor(path, "", "token-b", ""))
	if a == b {
		t.Fatal("two scoped tokens on one address shared a bucket")
	}
	agentHistory, historyRate, historyBurst := controller.keyAndBudget(contextFor(path, "node-a", "", ""))
	if agentHistory != "history:client:node-a" || historyRate != 2 || historyBurst != 5 {
		t.Fatal("Agent history inherited the reporting budget")
	}
	a, _, _ = controller.keyAndBudget(contextFor("/komari.network.v1.NetworkProbeService/SubmitPingProbeResult", "node-a", "", ""))
	b, _, _ = controller.keyAndBudget(contextFor("/komari.config.v1.ConfigService/AcknowledgeConfig", "node-a", "", ""))
	c, _, _ := controller.keyAndBudget(contextFor("/komari.config.v1.ConfigService/AcknowledgeConfig", "node-b", "", ""))
	if a == b || b == c {
		t.Fatal("probe traffic starved control traffic or nodes behind one NAT shared a bucket")
	}
	for i := 0; i < 200; i++ {
		controller.allow(first, rate, burst, time.Now())
	}
	if !controller.Update(config.ConfigEvent{Old: map[string]any{config.RateLimitEnabledKey: true}, New: map[string]any{config.RateLimitEnabledKey: false}}) {
		t.Fatal("live disable was not observed")
	}
	if allowed, _ := controller.allow(first, rate, burst, time.Now()); !allowed {
		t.Fatal("disabled mode retained an exhausted budget")
	}
}
