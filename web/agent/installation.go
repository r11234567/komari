package agent

import (
	"sync"
	"time"

	"github.com/komari-monitor/komari/database/metricstore"
	reportv1 "github.com/r11234567/komari-proto/gen/go/komari/report/v1"
	"google.golang.org/protobuf/proto"
)

type installationReport struct {
	observation *reportv1.InstallationObservation
	receivedAt  time.Time
	sequence    uint64
}

var installationReports = struct {
	sync.RWMutex
	byAgent map[string]installationReport
}{byAgent: make(map[string]installationReport)}

// SetConnectInstallation stores informational metadata, never authorization.
// It is intentionally separate from deployment templates and public reports.
func SetConnectInstallation(agentID string, sequence uint64, observation *reportv1.InstallationObservation) {
	if metricstore.EntityWritesBlocked(agentID) {
		return
	}
	installationReports.Lock()
	defer installationReports.Unlock()
	if observation == nil || observation.StartupId == "" || len(observation.StartupId) > 128 || observation.ObservedAt == nil || !observation.ObservedAt.IsValid() || proto.Size(observation) > 32<<10 {
		delete(installationReports.byAgent, agentID)
		return
	}
	previous := installationReports.byAgent[agentID]
	if previous.observation != nil && previous.observation.StartupId == observation.StartupId && sequence <= previous.sequence {
		return
	}
	copy := proto.Clone(observation).(*reportv1.InstallationObservation)
	copy.ProtoReflect().SetUnknown(nil)
	installationReports.byAgent[agentID] = installationReport{observation: copy, receivedAt: time.Now().UTC(), sequence: sequence}
}

func GetConnectInstallation(agentID string) (*reportv1.InstallationObservation, time.Time) {
	installationReports.RLock()
	defer installationReports.RUnlock()
	report := installationReports.byAgent[agentID]
	if report.observation == nil {
		return nil, time.Time{}
	}
	return proto.Clone(report.observation).(*reportv1.InstallationObservation), report.receivedAt
}

func clearConnectInstallation(agentID string) {
	installationReports.Lock()
	defer installationReports.Unlock()
	delete(installationReports.byAgent, agentID)
}
