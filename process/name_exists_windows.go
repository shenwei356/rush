//go:build windows

package process

import (
	"context"
	"errors"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Exists matches executable names without opening other processes,
// so processes owned by another user are not lost to handle-access errors.
func (m *NameMatcher) Exists(ctx context.Context) (bool, error) {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return false, err
	}
	defer windows.CloseHandle(snapshot)
	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	err = windows.Process32First(snapshot, &entry)
	for err == nil {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if entry.ProcessID != uint32(os.Getpid()) && m.matches(windows.UTF16ToString(entry.ExeFile[:])) {
			return true, nil
		}
		entry.Size = uint32(unsafe.Sizeof(entry))
		err = windows.Process32Next(snapshot, &entry)
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return false, err
	}
	return false, nil
}
