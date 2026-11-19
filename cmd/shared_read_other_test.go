//go:build !windows

package cmd

import "os"

// readFileShared reads a file the way the test observer must: without taking part
// in any sharing rule, so it never blocks the atomic state replace.
func readFileShared(path string) ([]byte, error) { return os.ReadFile(path) }
