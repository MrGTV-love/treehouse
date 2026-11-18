//go:build !windows

package gitvcs

import (
	"fmt"
	"os"
	"syscall"

	"github.com/kunchenguid/treehouse/v3/internal/pathidentity"
)

func fileIdentity(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", fmt.Errorf("cannot determine file identity for %s", path)
	}
	return fmt.Sprintf("%x:%x", uint64(stat.Dev), uint64(stat.Ino)), nil
}

// physicalPath returns the resolved on-disk path of an existing path, so a
// boundary comparison sees through every alias spelling.
func physicalPath(path string) (string, error) {
	return pathidentity.Existing(path)
}
