package agent

import (
	"testing"

	reportv1 "github.com/r11234567/komari-proto/gen/go/komari/report/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestInstallationObservationIsIsolatedOrderedAndCleared(t *testing.T) {
	id := "installation-observation-test"
	t.Cleanup(func() { DeleteConnectedClients(id) })
	observation := &reportv1.InstallationObservation{StartupId: "boot", ObservedAt: timestamppb.Now(), Executable: `C:\Komari\agent.exe`}
	SetConnectInstallation(id, 2, observation)
	observation.Executable = "mutated input"
	got, received := GetConnectInstallation(id)
	if got == nil || got.Executable != `C:\Komari\agent.exe` || received.IsZero() {
		t.Fatal("observation is missing or shares caller memory")
	}
	got.Executable = "mutated output"
	SetConnectInstallation(id, 1, observation)
	got, _ = GetConnectInstallation(id)
	if got.Executable != `C:\Komari\agent.exe` {
		t.Fatal("old report replaced new observation")
	}
	SetConnectInstallation(id, 3, nil)
	if got, _ = GetConnectInstallation(id); got != nil {
		t.Fatal("legacy Agent inherited previous process metadata")
	}
	SetConnectInstallation(id, 4, observation)
	SetClientProtocolVersion(id, 2)
	if got, _ = GetConnectInstallation(id); got != nil {
		t.Fatal("transport downgrade retained observation")
	}
	SetConnectInstallation(id, 5, observation)
	DeleteConnectedClients(id)
	if got, _ = GetConnectInstallation(id); got != nil {
		t.Fatal("node deletion retained installation paths")
	}
}
