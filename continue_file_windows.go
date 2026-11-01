package main

import (
	"os"

	"golang.org/x/sys/windows"
)

// lockContinueFile covers both existing records and future appends. Windows
// releases the lock when this handle closes, including on process termination.
func lockContinueFile(f *os.File, readOnly bool) error {
	flags := uint32(windows.LOCKFILE_FAIL_IMMEDIATELY)
	if !readOnly {
		flags |= windows.LOCKFILE_EXCLUSIVE_LOCK
	}
	return windows.LockFileEx(windows.Handle(f.Fd()), flags, 0, ^uint32(0), ^uint32(0), &windows.Overlapped{})
}
