//go:build linux

package pathidentity

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"golang.org/x/sys/unix"
)

func diskSpelling(path string) (string, error) {
	resolved, err := enumeratedSpelling(path)
	if err == nil || !os.IsPermission(err) {
		return resolved, err
	}
	fd, openErr := unix.Open(path, unix.O_PATH|unix.O_CLOEXEC, 0)
	if openErr != nil {
		return "", fmt.Errorf("cannot determine filesystem spelling of %s: %v (open: %v)", path, err, openErr)
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		unix.Close(fd)
		return "", fmt.Errorf("cannot inspect filesystem spelling of %s", path)
	}
	defer file.Close()
	info, statErr := file.Stat()
	if statErr != nil {
		return "", fmt.Errorf("cannot inspect filesystem spelling of %s: %v", path, statErr)
	}
	resolved, readErr := os.Readlink("/proc/self/fd/" + strconv.Itoa(fd))
	if readErr != nil {
		return "", fmt.Errorf("cannot determine filesystem spelling of %s: %v (proc: %v)", path, err, readErr)
	}
	if !filepath.IsAbs(resolved) {
		return "", fmt.Errorf("nonabsolute filesystem spelling of %s: %q", path, resolved)
	}
	resolvedInfo, statErr := os.Stat(resolved)
	if statErr != nil || !os.SameFile(info, resolvedInfo) {
		return "", fmt.Errorf("cannot verify filesystem spelling of %s: %q", path, resolved)
	}
	return resolved, nil
}
