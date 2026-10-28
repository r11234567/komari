package trustedcredential

import "testing"

func TestDiagnosticsVerificationCountsAndBusySnapshot(t *testing.T) {
	s := New(Options{})
	s.pending["synthetic-private-challenge"] = &pending{busy: true}
	if v := s.DiagnosticCounts(); v["pending"] != 1 || v["busy"] != 1 {
		t.Fatalf("bad counts: %v", v)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if v := s.DiagnosticCounts(); v["snapshot_busy"] != 1 {
		t.Fatal("busy verification state did not return promptly")
	}
}
