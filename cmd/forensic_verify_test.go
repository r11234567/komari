//go:build !windows

package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/r11234567/komari-proto/diagnostics"
)

func TestOpenVerifiedForensicRecord(t *testing.T) {
	public, key, err := diagnostics.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key)
	data := []byte("synthetic QGA record")
	sealed, err := diagnostics.Seal(public, diagnostics.Header{Node: [16]byte{1}, Stream: [16]byte{2}, Sequence: 1}, data)
	if err != nil {
		t.Fatal(err)
	}
	keyPath, output := filepath.Join(t.TempDir(), "key"), filepath.Join(t.TempDir(), "record.json")
	if err := os.WriteFile(keyPath, key, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := openVerifiedForensicRecord(keyPath, output, sealed); err == nil {
		t.Fatal("accepted a readable decryption key")
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatal("failed decryption created output")
	}
	if err := os.Chmod(keyPath, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := openVerifiedForensicRecord(keyPath, output, sealed); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(output)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("invalid record permissions: %v", err)
	}
	got, err := os.ReadFile(output)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("record bytes changed: %v", err)
	}
	if err := openVerifiedForensicRecord(keyPath, output, sealed); !os.IsExist(err) {
		t.Fatalf("overwrote existing disclosure file: %v", err)
	}
}
