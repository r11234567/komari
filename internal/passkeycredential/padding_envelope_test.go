package passkeycredential

import (
	"testing"

	"github.com/go-webauthn/webauthn/webauthn"
)

func TestPadAccountFirstRecordsDropsOutOfEnvelopeDescriptors(t *testing.T) {
	envelope := []lengthSlot{
		{length: 16, count: 1},
		{length: 32, count: 2},
	}
	secret := []byte("0123456789abcdef0123456789abcdef")

	outZero := padAccountFirstRecords(nil, envelope, secret, "panel.example", "nobody")

	rareID := make([]byte, 64)
	for i := range rareID {
		rareID[i] = byte(i + 1)
	}
	realCreds := []webauthn.Credential{
		{ID: []byte("1234567890abcdef")},
		{ID: rareID},
	}
	padded := padAccountFirstRecords(realCreds, envelope, secret, "panel.example", "alice")
	if len(padded) != len(outZero) {
		t.Fatalf("padded count = %d, want %d", len(padded), len(outZero))
	}
	counts := map[int]int{}
	for _, rec := range padded {
		counts[len(rec.ID)]++
	}
	if counts[64] != 0 {
		t.Fatalf("out-of-envelope 64-byte credential leaked: counts=%v", counts)
	}
	if counts[16] != 1 || counts[32] != 2 {
		t.Fatalf("padded lengths = %v, want map[16:1 32:2]", counts)
	}
}
