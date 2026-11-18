//go:build windows

package gitvcs

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// fileBasicInfo mirrors FILE_BASIC_INFO.
type fileBasicInfo struct {
	CreationTime   int64
	LastAccessTime int64
	LastWriteTime  int64
	ChangeTime     int64
	FileAttributes uint32
	_              uint32
}

// fileChangeStamp identifies an open file by its volume serial and file index
// plus the NTFS ChangeTime, which a write updates even when the last-write time
// is set back, together with size and last-write time.
func fileChangeStamp(file *os.File) (string, error) {
	handle := windows.Handle(file.Fd())
	var byHandle windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &byHandle); err != nil {
		return "", err
	}
	var basic fileBasicInfo
	if err := windows.GetFileInformationByHandleEx(handle, windows.FileBasicInfo, (*byte)(unsafe.Pointer(&basic)), uint32(unsafe.Sizeof(basic))); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x:%x:%x:%d:%d:%d", byHandle.VolumeSerialNumber, byHandle.FileIndexHigh, byHandle.FileIndexLow,
		uint64(byHandle.FileSizeHigh)<<32|uint64(byHandle.FileSizeLow), basic.ChangeTime, basic.LastWriteTime), nil
}
