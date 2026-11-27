package diagnosticrecords

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/r11234567/komari-proto/diagnostics"
)

func TestArchiveRetainsFirstReceipt(t *testing.T) {
	public, signing, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	recipient, private, err := diagnostics.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	archive, err := Open(dir, signing)
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	header := diagnostics.Header{Node: [16]byte{1}, Stream: [16]byte{2}, Sequence: 1}
	record := []byte("synthetic plaintext for a sealed test record")
	sealed, err := diagnostics.Seal(recipient, header, record)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := archive.Put([16]byte{2}, sealed); !errors.Is(err, ErrIdentity) {
		t.Fatalf("agent impersonation got %v", err)
	}
	first, err := archive.Put(header.Node, sealed)
	if err != nil || first.Verify(public, sealed) != nil {
		t.Fatalf("sealed receipt failed: %v", err)
	}
	retry, err := archive.Put(header.Node, sealed)
	if err != nil || !retry.ReceivedAt.Equal(first.ReceivedAt) || !bytes.Equal(retry.Signature, first.Signature) {
		t.Fatalf("retry changed signed receipt: %v", err)
	}
	conflict, err := diagnostics.Seal(recipient, header, []byte("changed record"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := archive.Put(header.Node, conflict); !errors.Is(err, ErrConflict) {
		t.Fatalf("overwrite got %v", err)
	}
	disk, _, err := archive.Read(header)
	if err != nil || !bytes.Equal(disk, sealed) || bytes.Contains(disk, record) {
		t.Fatalf("archive modified the original ciphertext: %v", err)
	}
	_, recovered, err := diagnostics.Open(private, disk)
	if err != nil || !bytes.Equal(record, recovered) {
		t.Fatalf("decrypted record mismatch: %v", err)
	}
	packed, err := os.ReadFile(filepath.Join(dir, recordName(header)))
	if err != nil {
		t.Fatal(err)
	}
	headerOnDisk, signed, envelope, err := VerifyBundle(bytes.NewReader(packed), public)
	if err != nil || headerOnDisk.Node != header.Node || !bytes.Equal(envelope, sealed) || !bytes.Equal(signed.Signature, first.Signature) {
		t.Fatalf("verification of exported evidence failed: %v", err)
	}
	packed[len(packed)-8] ^= 1
	if _, _, _, err := VerifyBundle(bytes.NewReader(packed), public); err == nil {
		t.Fatal("accepted corrupted evidence archive")
	}
}
