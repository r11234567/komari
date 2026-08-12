package safepath

import (
	"path/filepath"
	"testing"
)

func TestJoinUnderRejectsEscapes(t *testing.T) {
	base := t.TempDir()
	if got, err := JoinUnder(base, filepath.Join("dist", "index.html")); err != nil || got != filepath.Join(base, "dist", "index.html") {
		t.Fatalf("safe path = %q, err=%v", got, err)
	}
	for _, name := range []string{"", "../outside", filepath.Join("..", "outside"), filepath.Join(base, "outside"), string([]byte{'a', 0, 'b'})} {
		if got, err := JoinUnder(base, name); err == nil {
			t.Fatalf("unsafe path %q accepted as %q", name, got)
		}
	}
}
