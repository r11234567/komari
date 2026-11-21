package credentialaction

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestRescueSpecDigestAndTargetsMatchCanonicalJSON(t *testing.T) {
	got := RescueSpecDigest([]string{"--probe", "net"}, 300, 256*1024, 2222)
	sum := sha256.Sum256([]byte(`["v1",["--probe","net"],300,262144,2222]`))
	want := hex.EncodeToString(sum[:])
	if got != want {
		t.Fatalf("RescueSpecDigest() = %q, want %q", got, want)
	}

	nilArgs := RescueSpecDigest(nil, 300, 256*1024, 0)
	emptyArgs := RescueSpecDigest([]string{}, 300, 256*1024, 0)
	if nilArgs != emptyArgs {
		t.Fatalf("nil vs [] arguments diverged: %q vs %q", nilArgs, emptyArgs)
	}

	targets := RescueTargets("agent-1", 4, nil, 300, 256*1024, 22)
	if len(targets) != 4 || targets[0] != "agent-1" || targets[1] != "4" || targets[2] != "22" || targets[3] != "spec:"+RescueSpecDigest(nil, 300, 256*1024, 22) {
		t.Fatalf("unexpected RescueTargets: %v", targets)
	}
}
