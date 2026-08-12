package safepath

import (
	"fmt"
	"path/filepath"
	"strings"
)

// JoinUnder resolves an untrusted relative name and proves that the resulting
// lexical path remains below base. Callers creating a fresh directory must
// also reject archive symlinks so the lexical guarantee cannot be bypassed.
func JoinUnder(base, name string) (string, error) {
	if name == "" || strings.IndexByte(name, 0) >= 0 || filepath.IsAbs(name) || filepath.VolumeName(name) != "" {
		return "", fmt.Errorf("invalid relative path %q", name)
	}
	absBase, err := filepath.Abs(base)
	if err != nil {
		return "", err
	}
	target, err := filepath.Abs(filepath.Join(absBase, filepath.Clean(filepath.FromSlash(name))))
	if err != nil {
		return "", err
	}
	relative, err := filepath.Rel(absBase, target)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes its base directory", name)
	}
	return target, nil
}
