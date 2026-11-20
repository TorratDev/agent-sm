//go:build windows

package main

import (
	"errors"
	"os"
	"syscall"
	"unsafe"
)

// LockFileEx flag values (not exposed by the syscall package).
const (
	lockFileFailImmediately = 0x00000001
	lockFileExclusiveLock   = 0x00000002
	errorLockViolation      = syscall.Errno(33)
)

var (
	kernel32DLL      = syscall.NewLazyDLL("kernel32.dll")
	procLockFileEx   = kernel32DLL.NewProc("LockFileEx")
	procUnlockFileEx = kernel32DLL.NewProc("UnlockFileEx")
)

// lockFile takes an exclusive, non-blocking byte-range lock on f using
// LockFileEx, the Windows analogue of flock.
func lockFile(f *os.File) error {
	var ol syscall.Overlapped // byte range starts at offset 0
	r1, _, errno := procLockFileEx.Call(uintptr(f.Fd()),
		lockFileExclusiveLock|lockFileFailImmediately, 0, 1, 0,
		uintptr(unsafe.Pointer(&ol)))
	if r1 == 0 {
		return errno
	}
	return nil
}

func unlockFile(f *os.File) error {
	var ol syscall.Overlapped
	r1, _, errno := procUnlockFileEx.Call(uintptr(f.Fd()), 0, 1, 0,
		uintptr(unsafe.Pointer(&ol)))
	if r1 == 0 {
		return errno
	}
	return nil
}

func isLockHeld(err error) bool {
	return errors.Is(err, errorLockViolation)
}

// fsyncDir is a no-op: Windows cannot FlushFileBuffers on a directory handle,
// and NTFS already journals the directory metadata updates.
func fsyncDir(string) error { return nil }
