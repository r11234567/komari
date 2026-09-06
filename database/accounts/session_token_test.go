package accounts

import (
	"strings"
	"testing"
)

// The stored column must not be usable as a credential. If a digest read out of
// the sessions table authenticated, hashing would buy nothing: the leak it is
// meant to survive is exactly a read of that table.
func TestSessionDigestIsNotAcceptedAsAToken(t *testing.T) {
	if !isSessionDigest(sessionDigestPrefix + "deadbeef") {
		t.Fatal("a prefixed value must be recognized as a stored digest")
	}
	if isSessionDigest("MDEyMzQ1Njc4OWFiY2RlZg") {
		t.Fatal("a raw token must not be mistaken for a digest")
	}
	if isSessionDigest("") {
		t.Fatal("an empty value is not a digest")
	}
}

func TestHashSessionTokenIsStableAndDistinct(t *testing.T) {
	first := hashSessionToken("token-a")
	if first == "" || !strings.HasPrefix(first, sessionDigestPrefix) {
		t.Fatalf("digest %q must carry the version prefix", first)
	}
	if first == "token-a" || strings.Contains(first, "token-a") {
		t.Fatal("the digest must not embed the token")
	}
	if again := hashSessionToken("token-a"); again != first {
		t.Fatalf("digest is not stable: %q vs %q", again, first)
	}
	if other := hashSessionToken("token-b"); other == first {
		t.Fatal("different tokens must not collide")
	}
	// Already-hashed input passes through, so callers may hand over either form.
	if rehashed := hashSessionToken(first); rehashed != first {
		t.Fatalf("re-hashing a digest changed it: %q -> %q", first, rehashed)
	}
	if hashSessionToken("") != "" {
		t.Fatal("an empty token must hash to empty, not to a valid digest")
	}
}
