//go:build linux

package process

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	ps "github.com/shirou/gopsutil/process"
)

// linuxProcessName resolves names that may be truncated in /proc/PID/stat.
var linuxProcessName = func(ctx context.Context, pid int) (string, error) {
	return (&ps.Process{Pid: int32(pid)}).NameWithContext(ctx)
}

// fullPlatformProcessName resolves a full name without accepting a recycled PID.
func fullPlatformProcessName(p platformProcess) (string, error) {
	// /proc/PID/stat truncates comm to 15 bytes. Resolve longer names only
	// when executable exclusions require them, and reject PID reuse.
	if len(p.name) < 15 {
		return p.name, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	name, err := linuxProcessName(ctx, p.pid)
	if err != nil {
		if _, lookupErr := lookupPlatformProcess(p.pid); unixProcessGone(lookupErr) {
			return "", os.ErrNotExist
		}
		return "", err
	}
	current, err := lookupPlatformProcess(p.pid)
	if err != nil {
		return "", err
	}
	if current.identity != p.identity {
		return "", fmt.Errorf("process identity changed for pid %d", p.pid)
	}
	if name == "" {
		return "", fmt.Errorf("empty executable name for pid %d", p.pid)
	}
	return filepath.Base(name), nil
}
