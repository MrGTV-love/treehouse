//go:build darwin

package pathidentity

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

func diskSpelling(path string) (string, error) {
	name, err := unix.BytePtrFromString(path)
	if err != nil {
		return "", err
	}
	request := unix.Attrlist{Bitmapcount: 5,
		Commonattr: unix.ATTR_CMN_RETURNED_ATTRS | unix.ATTR_CMN_FULLPATH}
	var buf [unix.PathMax + 32]byte
	_, _, errno := unix.Syscall6(unix.SYS_GETATTRLIST, uintptr(unsafe.Pointer(name)),
		uintptr(unsafe.Pointer(&request)), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), 0, 0)
	runtime.KeepAlive(name)
	runtime.KeepAlive(request)
	if errno != 0 {
		return "", &os.PathError{Op: "getattrlist", Path: path, Err: errno}
	}
	length := int64(binary.LittleEndian.Uint32(buf[:4]))
	if length < 32 || length > int64(len(buf)) ||
		binary.LittleEndian.Uint32(buf[4:8])&request.Commonattr != request.Commonattr {
		return "", fmt.Errorf("filesystem spelling attributes unavailable for %s", path)
	}
	offset := int64(24) + int64(int32(binary.LittleEndian.Uint32(buf[24:28])))
	size := int64(binary.LittleEndian.Uint32(buf[28:32]))
	if offset < 32 || size < 2 || offset > length || size > length-offset || buf[offset+size-1] != 0 {
		return "", fmt.Errorf("invalid filesystem spelling attributes for %s", path)
	}
	resolved := string(buf[offset : offset+size-1])
	if !filepath.IsAbs(resolved) {
		return "", fmt.Errorf("nonabsolute filesystem spelling of %s: %q", path, resolved)
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	resolvedInfo, err := os.Stat(resolved)
	if err != nil || !os.SameFile(info, resolvedInfo) {
		return "", fmt.Errorf("cannot verify filesystem spelling of %s: %q", path, resolved)
	}
	return resolved, nil
}
