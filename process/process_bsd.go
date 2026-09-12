//go:build darwin || freebsd || openbsd

package process

import (
	"context"
	"errors"
	"os"
	"syscall"
	"time"

	ps "github.com/shirou/gopsutil/process"
)

const bsdEnumerationTimeout = 2 * time.Second

func snapshotPlatformProcesses() (map[int]platformProcess, error) {
	ctx, cancel := context.WithTimeout(context.Background(), bsdEnumerationTimeout)
	defer cancel()
	items, err := ps.ProcessesWithContext(ctx)
	if err != nil {
		return nil, err
	}
	result := make(map[int]platformProcess, len(items))
	for _, item := range items {
		p, err := readBSDProcess(ctx, item)
		if err == nil && p.identity != 0 {
			result[p.pid] = p
		}
	}
	return result, nil
}
func lookupPlatformProcess(pid int) (platformProcess, error) {
	ctx, cancel := context.WithTimeout(context.Background(), bsdEnumerationTimeout)
	defer cancel()
	p, err := readBSDProcess(ctx, &ps.Process{Pid: int32(pid)})
	if errors.Is(err, syscall.ESRCH) {
		return platformProcess{}, os.ErrNotExist
	}
	return p, err
}
func readBSDProcess(ctx context.Context, item *ps.Process) (platformProcess, error) {
	ppid, err := item.PpidWithContext(ctx)
	if err != nil {
		return platformProcess{}, err
	}
	name, err := item.NameWithContext(ctx)
	if err != nil {
		return platformProcess{}, err
	}
	created, err := item.CreateTimeWithContext(ctx)
	if err != nil {
		return platformProcess{}, err
	}
	pgid, err := syscall.Getpgid(int(item.Pid))
	if err != nil {
		return platformProcess{}, err
	}
	return platformProcess{pid: int(item.Pid), ppid: int(ppid), pgid: pgid, identity: uint64(created), name: name}, nil
}
