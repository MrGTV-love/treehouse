//go:build linux

package gitvcs

import (
	"fmt"
	"os"
	"syscall"
)

// fileChangeStamp identifies an open file by what a write cannot restore:
// device, inode and the inode change time (ctime), plus size and modification
// time. A writer can reset the modification time but not the change time.
func fileChangeStamp(file *os.File) (string, error) {
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", fmt.Errorf("cannot determine change time of %s", file.Name())
	}
	return fmt.Sprintf("%x:%x:%d:%d.%09d:%d.%09d", uint64(stat.Dev), uint64(stat.Ino), stat.Size,
		int64(stat.Ctim.Sec), int64(stat.Ctim.Nsec), int64(stat.Mtim.Sec), int64(stat.Mtim.Nsec)), nil
}
