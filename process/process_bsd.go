//go:build freebsd || openbsd

package process

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

func snapshotPlatformProcesses() (map[int]platformProcess, error) {
	items, err := bsdKernelProcesses(0)
	if err != nil {
		return nil, err
	}
	result := make(map[int]platformProcess, len(items))
	for _, p := range items {
		if p.pid > 0 && p.identity != 0 {
			result[p.pid] = p
		}
	}
	return result, nil
}

func lookupPlatformProcess(pid int) (platformProcess, error) {
	items, err := bsdKernelProcesses(pid)
	if err == syscall.ESRCH || err == syscall.ENOENT {
		return platformProcess{}, os.ErrNotExist
	}
	if err != nil {
		return platformProcess{}, err
	}
	if len(items) != 1 || items[0].pid != pid || items[0].identity == 0 {
		return platformProcess{}, os.ErrNotExist
	}
	return items[0], nil
}

// bsdSysctl reads a complete kernel process table. OpenBSD's MIB includes
// record size and capacity; FreeBSD's does not.
func bsdSysctl(mib []int32, recordSize int, openbsd bool) ([]byte, error) {
	for attempt := 0; attempt < 3; attempt++ {
		var length uint64
		_, _, errno := unix.Syscall6(unix.SYS___SYSCTL,
			uintptr(unsafe.Pointer(&mib[0])), uintptr(len(mib)), 0,
			uintptr(unsafe.Pointer(&length)), 0, 0)
		if errno != 0 {
			return nil, errno
		}
		if length == 0 {
			return nil, nil
		}
		length += length/4 + uint64(recordSize*16)
		buf := make([]byte, length)
		if openbsd {
			mib[len(mib)-1] = int32(len(buf) / recordSize)
		}
		_, _, errno = unix.Syscall6(unix.SYS___SYSCTL,
			uintptr(unsafe.Pointer(&mib[0])), uintptr(len(mib)),
			uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&length)), 0, 0)
		runtime.KeepAlive(mib)
		if errno == syscall.ENOMEM {
			continue
		}
		if errno != 0 {
			return nil, errno
		}
		if length%uint64(recordSize) != 0 {
			return nil, fmt.Errorf("kernel process table has %d bytes, not a multiple of %d", length, recordSize)
		}
		return buf[:length], nil
	}
	return nil, syscall.ENOMEM
}

func bsdUint32(record []byte, offset uintptr) int {
	return int(binary.LittleEndian.Uint32(record[offset:]))
}

func bsdUint64(record []byte, offset uintptr) uint64 {
	return binary.LittleEndian.Uint64(record[offset:])
}

func bsdName(record []byte, offset, length uintptr) string {
	return string(bytes.TrimRight(record[offset:offset+length], "\x00"))
}
