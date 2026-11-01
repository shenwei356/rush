package main

import (
	"fmt"
	"os"
)

// openContinueFile holds a lock until Close so another run cannot append records
// that this run would erase when rolling back after an output or write failure.
// Dry runs only read existing records and never create the file.
func openContinueFile(path string, dryRun bool) (*os.File, error) {
	// O_APPEND omits FILE_WRITE_DATA on Windows, preventing rollback by
	// truncation. The exclusive lock protects our read-then-append sequence.
	flags := os.O_RDWR | os.O_CREATE
	if dryRun {
		flags = os.O_RDONLY
	}
	f, err := os.OpenFile(path, flags, 0666)
	if err != nil {
		if dryRun && os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	info, err := f.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = fmt.Errorf("successful-command file must be a regular file: %s", path)
	}
	if err == nil {
		err = lockContinueFile(f, dryRun)
		if err != nil {
			err = fmt.Errorf("lock successful-command file %q (it may be in use by another rush): %w", path, err)
		}
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}
