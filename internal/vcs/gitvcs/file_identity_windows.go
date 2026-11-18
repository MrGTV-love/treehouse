//go:build windows

package gitvcs

import (
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/windows"
)

func fileIdentity(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &info); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x:%x:%x", info.VolumeSerialNumber, info.FileIndexHigh, info.FileIndexLow), nil
}

// physicalPath returns the path the opened object really has on its volume.
// filepath.EvalSymlinks keeps junctions (mount points) and SUBST drive letters,
// so it cannot prove that two spellings name different places; the final path
// of an open handle resolves both.
func physicalPath(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	buf := make([]uint16, windows.MAX_LONG_PATH)
	// Flags 0 = FILE_NAME_NORMALIZED|VOLUME_NAME_DOS: a drive-letter path.
	n, err := windows.GetFinalPathNameByHandle(windows.Handle(file.Fd()), &buf[0], uint32(len(buf)), 0)
	if err != nil {
		return "", err
	}
	if n >= uint32(len(buf)) {
		return "", fmt.Errorf("final path too long: %s", path)
	}
	final := windows.UTF16ToString(buf[:n])
	if rest, ok := strings.CutPrefix(final, `\\?\UNC\`); ok {
		return `\\` + rest, nil
	}
	return strings.TrimPrefix(final, `\\?\`), nil
}
