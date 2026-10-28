package workspace

import (
	"testing"
	"time"
)

func TestDiagnosticsWorkspaceSnapshotNeverValidatesOrWaits(t *testing.T) {
	s := NewStore(time.Now, func(Binding) bool { t.Fatal("diagnostics attempted session validation"); return false })
	s.grants[[32]byte{1}] = &Grant{Binding: Binding{SystemUser: "synthetic-private-user"}}
	if v := s.DiagnosticCounts(); v["stored_grants"] != 1 {
		t.Fatalf("bad counts: %v", v)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if v := s.DiagnosticCounts(); v["snapshot_busy"] != 1 {
		t.Fatal("busy workspace state did not return promptly")
	}
}
