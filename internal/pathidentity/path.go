// Package pathidentity resolves directory spellings without assuming that a
// filesystem is case-insensitive. Existing paths use the names stored on disk;
// absent suffixes retain their requested spelling.
package pathidentity

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Prefix resolves symlinks and the filesystem's spelling of the deepest
// existing ancestor. It does not lowercase names or relocate directories.
func Prefix(path string) (string, error) {
	current, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	missing := ""
	for {
		resolved, err := Existing(current)
		if err == nil {
			return filepath.Join(resolved, missing), nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf("no existing directory found above %s", path)
		}
		missing = filepath.Join(filepath.Base(current), missing)
		current = parent
	}
}

// Existing returns one spelling for an existing path, including case aliases
// on APFS/NTFS and symlinks. EvalSymlinks alone retains input case on Unix.
func Existing(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	return diskSpelling(resolved)
}

func diskSpelling(path string) (string, error) {
	parent := filepath.Dir(path)
	if parent == path {
		return path, nil
	}
	canonicalParent, err := diskSpelling(parent)
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
