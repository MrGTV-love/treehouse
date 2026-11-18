//go:build windows

package pool

import (
	"io"
	"io/fs"
	"os"

	"golang.org/x/sys/windows"
)

// ReadFileShared reads a file with FILE_SHARE_DELETE as well as read and write
// sharing. Go's os.ReadFile opens without FILE_SHARE_DELETE, and while such a
// handle is open the state commit (ReplaceFileW in state_file_windows.go) fails
// with a sharing violation. A reader that must not disturb a writer, such as
// the recovered-entry snapshot of `return --all` or a test observer, uses this.
func ReadFileShared(path string) ([]byte, error) {
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
