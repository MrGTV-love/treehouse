//go:build !darwin && !windows

package pathidentity

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func enumeratedSpelling(path string) (string, error) {
	parent := filepath.Dir(path)
	if parent == path {
		return path, nil
	}
	canonicalParent, err := enumeratedSpelling(parent)
	if err != nil {
		return "", err
	}
	entries, err := os.ReadDir(canonicalParent)
	if err != nil {
		return "", err
	}
	base := filepath.Base(path)
	for _, entry := range entries {
		if entry.Name() == base {
			return filepath.Join(canonicalParent, base), nil
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	// EqualFold is only a search optimization, never the identity decision.
	// The fallback also handles filesystem Unicode normalization rules that
	// differ from Go's case folding.
	for _, folded := range []bool{true, false} {
		for _, entry := range entries {
			if strings.EqualFold(entry.Name(), base) != folded {
				continue
			}
			candidate := filepath.Join(canonicalParent, entry.Name())
			candidateInfo, err := os.Lstat(candidate)
			if err == nil && os.SameFile(info, candidateInfo) {
				return candidate, nil
			}
		}
	}
	return "", fmt.Errorf("cannot determine filesystem spelling of %s", path)
}
