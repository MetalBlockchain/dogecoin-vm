//go:build unix

package main

import (
	"os"
	"syscall"
)

// lockFile takes an exclusive lock on f, waiting for it.
func lockFile(f *os.File) error {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if err != syscall.EINTR {
			return err
		}
	}
}

func unlockFile(f *os.File) error { return syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }

// tryLockFile takes an exclusive lock on f without waiting: errLocked if
// another process holds it.
func tryLockFile(f *os.File) error {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		switch err {
		case syscall.EINTR:
			continue
		case syscall.EWOULDBLOCK:
			return errLocked
		}
		return err
	}
}
