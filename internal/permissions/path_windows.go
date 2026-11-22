//go:build windows

package permissions

import (
	"path/filepath"
	"syscall"
	"unsafe"
)

var (
	modKernel32                  = syscall.NewLazyDLL("kernel32.dll")
	procGetFinalPathNameByHandle = modKernel32.NewProc("GetFinalPathNameByHandleW")
)

// evalRealPath resolves symlinks via filepath.EvalSymlinks AND resolves NTFS directory junctions,
// mount points, and 8.3 short names via GetFinalPathNameByHandleW.
func evalRealPath(p string) (string, error) {
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", err
	}

	pathPtr, err := syscall.UTF16PtrFromString(resolved)
	if err != nil {
		return resolved, nil
	}

	h, err := syscall.CreateFile(
		pathPtr,
		0,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil,
		syscall.OPEN_EXISTING,
		syscall.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return resolved, nil
	}
	defer syscall.CloseHandle(h)

	buf := make([]uint16, 1024)
	r0, _, _ := procGetFinalPathNameByHandle.Call(
		uintptr(h),
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(len(buf)),
		0, // FILE_NAME_NORMALIZED | VOLUME_NAME_DOS
	)
	n := uint32(r0)
	if n == 0 {
		return resolved, nil
	}
	if n >= uint32(len(buf)) {
		buf = make([]uint16, n+1)
		r0, _, _ = procGetFinalPathNameByHandle.Call(
			uintptr(h),
			uintptr(unsafe.Pointer(&buf[0])),
			uintptr(len(buf)),
			0,
		)
		n = uint32(r0)
		if n == 0 || n >= uint32(len(buf)) {
			return resolved, nil
		}
	}
	return normalizeExtendedPath(syscall.UTF16ToString(buf[:n])), nil
}
