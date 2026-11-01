//go:build !windows

package main

import (
	"os"

	"golang.org/x/sys/unix"
)

// lockContinueFile fails immediately on conflict and releases on file close.
func lockContinueFile(f *os.File, readOnly bool) error {
	mode := unix.LOCK_EX
	if readOnly {
		mode = unix.LOCK_SH
	}
	return unix.Flock(int(f.Fd()), mode|unix.LOCK_NB)
}
