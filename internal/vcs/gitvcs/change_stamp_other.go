//go:build !windows && !linux && !darwin && !freebsd && !netbsd && !dragonfly

package gitvcs

import (
	"fmt"
	"os"
)

// fileChangeStamp has no change-time source on this platform, so the content
// comparison fails closed: a seeded file is never judged regenerable here.
func fileChangeStamp(file *os.File) (string, error) {
	return "", fmt.Errorf("no inode change time available to authenticate %s", file.Name())
}
