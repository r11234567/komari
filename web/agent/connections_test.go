package agent

import (
	"testing"
	"time"

	v1 "github.com/komari-monitor/komari/protocol/v1"
	v2 "github.com/komari-monitor/komari/protocol/v2"
	"github.com/komari-monitor/komari/web/connection"
)

func TestRecordReportKeepsLatestAndShortRecentWindow(t *testing.T) {
	mu.Lock()
	previousLatest := latestReport
	previousRecent := recentReports
	latestReport = make(map[string]*v1.Report)
	recentReports = make(map[string][]v1.Report)
	mu.Unlock()
	t.Cleanup(func() {
		mu.Lock()
		latestReport = previousLatest
		recentReports = previousRecent
		mu.Unlock()
	})

	now := time.Now().UTC()
	RecordReport(v1.Report{UUID: "node-a", UpdatedAt: now.Add(-2 * time.Minute), CPU: v1.CPUReport{Usage: 10}})
	RecordReport(v1.Report{UUID: "node-a", UpdatedAt: now.Add(-30 * time.Second), CPU: v1.CPUReport{Usage: 20}})
	RecordReport(v1.Report{UUID: "node-a", UpdatedAt: now.Add(-45 * time.Second), CPU: v1.CPUReport{Usage: 15}})

	recent := GetRecentReports("node-a")
	if len(recent) != 2 || recent[0].CPU.Usage != 15 || recent[1].CPU.Usage != 20 {
		t.Fatalf("recent reports = %#v", recent)
	}
	recent[0].CPU.Usage = 99
	if got := GetRecentReports("node-a"); len(got) != 2 || got[0].CPU.Usage != 15 {
		t.Fatalf("recent report cache was mutated through returned slice: %#v", got)
	}

	latest := GetLatestReport()
	if latest["node-a"] == nil || latest["node-a"].CPU.Usage != 20 {
		t.Fatalf("latest report = %#v", latest["node-a"])
	}
	latest["node-a"].CPU.Usage = 99
	if got := GetLatestReport()["node-a"]; got == nil || got.CPU.Usage != 20 {
		t.Fatalf("latest report cache was mutated through returned map: %#v", got)
	}

	DeleteLatestReport("node-a")
	if len(GetRecentReports("node-a")) != 0 || GetLatestReport()["node-a"] != nil {
		t.Fatal("deleting latest report did not clear runtime report state")
	}
}

func TestDeleteConnectedClientsClearsAllRuntimeState(t *testing.T) {
	mu.Lock()
	previousConnected := connectedClients
	previousProtocols := connectedClientProtocol
	previousCapabilities := v2Capabilities
	previousPresence := presenceOnly
	previousLatest := latestReport
	previousRecent := recentReports
	connectedClients = make(map[string]*connection.SafeConn)
	connectedClientProtocol = make(map[string]int)
	v2Capabilities = make(map[string]map[string]bool)
	presenceOnly = make(map[string]struct {
		id     int64
		expire time.Time
	})
	latestReport = make(map[string]*v1.Report)
	recentReports = make(map[string][]v1.Report)
	mu.Unlock()
	t.Cleanup(func() {
		mu.Lock()
		connectedClients = previousConnected
		connectedClientProtocol = previousProtocols
		v2Capabilities = previousCapabilities
		presenceOnly = previousPresence
		latestReport = previousLatest
		recentReports = previousRecent
		mu.Unlock()
	})
	v2EventMu.Lock()
	previousQueues := v2EventQueues
	v2EventQueues = make(map[string]*v2EventQueue)
	v2EventMu.Unlock()
	t.Cleanup(func() {
		v2EventMu.Lock()
		v2EventQueues = previousQueues
		v2EventMu.Unlock()
	})

	SetClientProtocolVersion("node-a", 2)
	KeepAlivePresence("node-a", 42, time.Minute)
	RecordReport(v1.Report{UUID: "node-a", UpdatedAt: time.Now().UTC()})
	EnqueueV2Event("node-a", v2.MethodAgentExec, v2.ExecParams{TaskID: "task"})
	DeleteConnectedClients("node-a")

	if IsAgentOnline("node-a") || GetLatestReport()["node-a"] != nil || len(GetRecentReports("node-a")) != 0 {
		t.Fatal("deleted client still has online or report state")
	}
	if events := TakeV2Events("node-a", nil, 16); len(events) != 0 {
		t.Fatalf("deleted client still has queued events: %#v", events)
	}
}

func TestV2ConfigRequiresExplicitCapability(t *testing.T) {
	mu.Lock()
	previousProtocols := connectedClientProtocol
	previousCapabilities := v2Capabilities
	connectedClientProtocol = make(map[string]int)
	v2Capabilities = make(map[string]map[string]bool)
	mu.Unlock()
	t.Cleanup(func() {
		mu.Lock()
		connectedClientProtocol = previousProtocols
		v2Capabilities = previousCapabilities
		mu.Unlock()
	})

	SetClientProtocolVersion("legacy", 2)
	SetV2Capabilities("legacy", []string{"ping", "terminal"})
	if SupportsV2Config("legacy") || !IsV2ConfigUpgradeRequired("legacy") {
		t.Fatal("legacy v2 Agent without agent.config was treated as config-capable")
	}

	SetV2Capabilities("legacy", []string{"ping", v2.MethodAgentConfig})
	if !SupportsV2Config("legacy") || IsV2ConfigUpgradeRequired("legacy") {
		t.Fatal("explicit agent.config capability was not honored")
	}

	SetClientProtocolVersion("connect", 3)
	SetV2Capabilities("connect", []string{v2.MethodAgentConfig})
	if !IsConnectClient("connect") || IsV2Client("connect") || SupportsV2Config("connect") || IsV2ConfigUpgradeRequired("connect") {
		t.Fatal("Connect Agent leaked into the legacy v2 capability adapter")
	}
}

func TestReturnRouteRequiresExplicitTransportCapability(t *testing.T) {
	mu.Lock()
	previousProtocols := connectedClientProtocol
	previousV2Capabilities := v2Capabilities
	previousConnectCapabilities := connectCapabilities
	connectedClientProtocol = make(map[string]int)
	v2Capabilities = make(map[string]map[string]bool)
	connectCapabilities = make(map[string]map[string]bool)
	mu.Unlock()
	t.Cleanup(func() {
		mu.Lock()
		connectedClientProtocol = previousProtocols
		v2Capabilities = previousV2Capabilities
		connectCapabilities = previousConnectCapabilities
		mu.Unlock()
	})

	SetClientProtocolVersion("legacy", 2)
	SetV2Capabilities("legacy", []string{"ping"})
	if SupportsReturnRoute("legacy") {
		t.Fatal("legacy Agent without route capability was accepted")
	}
	SetV2Capabilities("legacy", []string{v2.MethodAgentRoute})
	if !SupportsReturnRoute("legacy") {
		t.Fatal("legacy Agent route capability was ignored")
	}

	SetClientProtocolVersion("connect", 3)
	if SupportsReturnRoute("connect") {
		t.Fatal("Connect Agent without typed capability was accepted")
	}
	SetConnectCapabilities("connect", true)
	if !SupportsReturnRoute("connect") {
		t.Fatal("Connect Agent typed capability was ignored")
	}
}

func TestReturnRouteLeaseRestoresConnectCapability(t *testing.T) {
	mu.Lock()
	previousProtocols := connectedClientProtocol
	previousCapabilities := connectCapabilities
	connectedClientProtocol = make(map[string]int)
	connectCapabilities = make(map[string]map[string]bool)
	mu.Unlock()
	t.Cleanup(func() {
		mu.Lock()
		connectedClientProtocol = previousProtocols
		connectCapabilities = previousCapabilities
		mu.Unlock()
	})

	MarkConnectReturnRouteLease("connect")
	if !IsConnectClient("connect") || !SupportsReturnRoute("connect") {
		t.Fatal("authenticated Connect route lease did not restore probe capability")
	}
}
