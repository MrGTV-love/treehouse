//go:build !windows

package pool

import "os"

// ReadFileShared reads a file without taking part in any sharing rule, so a
// reader never blocks the atomic state replace. Unix has no such rule.
func ReadFileShared(path string) ([]byte, error) {
	return os.ReadFile(path)
}
