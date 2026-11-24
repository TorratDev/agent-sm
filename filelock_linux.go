package main

import (
	"errors"
	"os"
	"syscall"
)

// lockFile takes an exclusive, non-blocking advisory lock on f.
func lockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

func unlockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}

// isLockHeld reports whether a lockFile failure means another holder exists.
func isLockHeld(err error) bool {
	return errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN)
}

// fsyncDir flushes a directory's metadata so file creations in it survive a crash.
func fsyncDir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
