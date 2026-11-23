package secureconfig

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRewrapForRestoreUsesTheStagedKey(t *testing.T) {
	t.Setenv("KOMARI_SECRET_KEY", "")
	liveDir, stageDir := t.TempDir(), t.TempDir()
	const liveMaterial = "synthetic-live-sealing-key"
	const stageMaterial = "synthetic-backup-sealing-key"
	for dir, material := range map[string]string{liveDir: liveMaterial, stageDir: stageMaterial} {
		if err := os.WriteFile(filepath.Join(dir, filepath.Base(keyFilePath)), []byte(material), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	liveKey, err := normalizeKey(liveMaterial)
	if err != nil {
		t.Fatal(err)
	}
	stagedKey, err := normalizeKey(stageMaterial)
	if err != nil {
		t.Fatal(err)
	}
	in, err := encryptWithKey("synthetic-recovery-state", liveKey)
	if err != nil {
		t.Fatal(err)
	}
	out, err := RewrapForRestore(in, liveDir, stageDir)
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := decryptWithKey(out, stagedKey)
	if err != nil || plaintext != "synthetic-recovery-state" {
		t.Fatal("staged key cannot open the restored state")
	}
	if _, err := decryptWithKey(out, liveKey); err == nil {
		t.Fatal("restored state was still encrypted with the live key")
	}
}
