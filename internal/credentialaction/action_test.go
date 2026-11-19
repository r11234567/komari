package credentialaction

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestExecutionSpecDigestMatchesFrontendGoldenVector(t *testing.T) {
	expectedPayload := `["v1","echo <ok&>",["-n","a\u2028b"],[["A_VAR","1"],["Z_VAR","2"]],"/srv/app",30,4096,true]`
	sum := sha256.Sum256([]byte(expectedPayload))
	expectedHex := hex.EncodeToString(sum[:])

	digest := ExecutionSpecDigest(
		"echo <ok&>",
		[]string{"-n", "a\u2028b"},
		map[string]string{"Z_VAR": "2", "A_VAR": "1"},
		"/srv/app",
		30,
		4096,
		true,
	)
	require.Equal(t, expectedHex, digest)

	// Reordered env map must produce identical digest.
	digest2 := ExecutionSpecDigest(
		"echo <ok&>",
		[]string{"-n", "a\u2028b"},
		map[string]string{"A_VAR": "1", "Z_VAR": "2"},
		"/srv/app",
		30,
		4096,
		true,
	)
	require.Equal(t, expectedHex, digest2)

	// Altered command must produce a different digest.
	require.NotEqual(t, expectedHex, ExecutionSpecDigest(
		"rm -rf /",
		[]string{"-n", "a\u2028b"},
		map[string]string{"A_VAR": "1", "Z_VAR": "2"},
		"/srv/app",
		30,
		4096,
		true,
	))

	require.Equal(t, []string{"agent-a", "run-as:root", "spec:" + expectedHex}, ExecutionTargets(
		[]string{"agent-a"},
		"echo <ok&>",
		[]string{"-n", "a\u2028b"},
		map[string]string{"Z_VAR": "2", "A_VAR": "1"},
		"/srv/app",
		30,
		4096,
		true,
	))
}
