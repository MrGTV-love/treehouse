//go:build windows

package cmd

import (
	"io"
	"io/fs"
	"os"

	"golang.org/x/sys/windows"
)

// readFileShared opens with FILE_SHARE_DELETE as well as read and write
// sharing. Go's os.ReadFile does not, and while such a handle is open the state
// commit (ReplaceFileW) fails with a sharing violation, which would fail the
// racing commands for a test-only reason.
func readFileShared(path string) ([]byte, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	file := os.NewFile(uintptr(handle), path)
	defer file.Close()
	return io.ReadAll(file)
}
