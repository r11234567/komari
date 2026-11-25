//go:build !windows

package cloudflared

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveBinaryPathRejectsDataDirectory(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	dataDir := filepath.Join(root, "data")
	binDir := filepath.Join(root, "bin")
	for _, dir := range []string{dataDir, binDir} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	dataBinary := filepath.Join(dataDir, "cloudflared")
	trustedBinary := filepath.Join(binDir, "cloudflared")
	for _, binary := range []string{dataBinary, trustedBinary} {
		if err := os.WriteFile(binary, []byte("fixture"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	alias := filepath.Join(binDir, "alias")
	if err := os.Symlink(dataBinary, alias); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dataDir+string(os.PathListSeparator)+binDir)
	for _, candidate := range []string{"", dataBinary, alias} {
		t.Setenv("KOMARI_CLOUDFLARED_BIN", candidate)
		if found, _ := resolveBinaryPath(); found == dataBinary {
			t.Fatalf("resolved a data directory binary through %q", candidate)
		}
	}
	t.Setenv("KOMARI_CLOUDFLARED_BIN", trustedBinary)
	if found, ok := resolveBinaryPath(); !ok || found != trustedBinary {
		t.Fatalf("trusted binary lookup = %q, %t", found, ok)
	}
}
