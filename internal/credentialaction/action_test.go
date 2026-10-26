package credentialaction

import "testing"

func TestPublicTargetScopeEncoding(t *testing.T) {
	// SHA-256 of the standard empty JSON array; the Web client uses the same
	// vector. This is a target checksum, not credential/password hashing.
	const empty = "action:example:4f53cda18c2baa0c0354bb5f9a3ecbe5ed12ab4d8e11ba873c2f11161202b945"
	if actual := ScopedResource("example"); actual != empty {
		t.Fatalf("scope vector: %s", actual)
	}
	if actual := WorkspaceResource("node", "alice"); actual != `workspace:["node","alice"]` {
		t.Fatalf("workspace scope: %s", actual)
	}
}
