//go:build windows

package cmd

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

// readFileShared opens with FILE_SHARE_DELETE as well as read and write
// sharing. Go's os.ReadFile does not, and while such a handle is open the state
// commit (ReplaceFileW) fails with a sharing violation, which would fail the
// racing commands for a test-only reason.
//
// ReplaceFileW is not atomic for a reader that holds no state lock: for a short
// time the name is missing, or held without sharing. Such an open is retried
// for a bounded time; a file that stays missing or locked is still an error.
func readFileShared(path string) ([]byte, error) {
	deadline := time.Now().Add(2 * time.Second)
	for {
		data, err := readFileSharedOnce(path)
		if err == nil || !isTransientReplaceError(err) || time.Now().After(deadline) {
			return data, err
		}
		time.Sleep(time.Millisecond)
	}
}

func isTransientReplaceError(err error) bool {
	return errors.Is(err, windows.ERROR_FILE_NOT_FOUND) ||
		errors.Is(err, windows.ERROR_SHARING_VIOLATION) ||
		errors.Is(err, windows.ERROR_ACCESS_DENIED)
}

func readFileSharedOnce(path string) ([]byte, error) {
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
