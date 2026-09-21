//go:build darwin

package process

import (
	"bytes"
	"os"

	"golang.org/x/sys/unix"
)

func snapshotPlatformProcesses() (map[int]platformProcess, error) {
	items, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, err
	}
	result := make(map[int]platformProcess, len(items))
	for i := range items {
		p := darwinProcess(&items[i])
		if p.pid > 0 && p.identity != 0 {
			result[p.pid] = p
		}
	}
	return result, nil
}

func lookupPlatformProcess(pid int) (platformProcess, error) {
	items, err := unix.SysctlKinfoProcSlice("kern.proc.pid", pid)
	if err != nil {
		return platformProcess{}, err
	}
	if len(items) == 0 {
		return platformProcess{}, os.ErrNotExist
	}
	p := darwinProcess(&items[0])
	if p.pid != pid || p.identity == 0 {
		return platformProcess{}, os.ErrNotExist
	}
	return p, nil
}

func darwinProcess(item *unix.KinfoProc) platformProcess {
	start := item.Proc.P_starttime
	return platformProcess{
		pid:      int(item.Proc.P_pid),
		ppid:     int(item.Eproc.Ppid),
		pgid:     int(item.Eproc.Pgid),
		identity: uint64(start.Sec)*1_000_000 + uint64(start.Usec),
		name:     string(bytes.TrimRight(item.Proc.P_comm[:], "\x00")),
		zombie:   item.Proc.P_stat == 5, // SZOMB in sys/proc.h
	}
}
