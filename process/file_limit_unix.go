//go:build !windows

package process

import (
	"fmt"
	"os"
	"syscall"
)

// fileDescriptorJobLimit budgets descriptors for a complete job, including
// buffered retry output retained until that job's output has been consumed.
func fileDescriptorJobLimit(opts *Options) (int, error) {
	if opts.DryRun {
		return opts.Jobs, nil
	}
	var limit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &limit); err != nil {
		return 0, fmt.Errorf("read open-file limit: %w", err)
	}
	// Rlimit.Cur is signed on some BSDs and unsigned on Linux/macOS.
	softLimit := uint64(limit.Cur)
	// Include inherited descriptors and the output/continue files. On systems
	// without /dev/fd, leave a conservative allowance for these existing files.
	opened := uint64(32)
	if entries, err := os.ReadDir("/dev/fd"); err == nil {
		opened = uint64(len(entries))
	}
	// Reserve room for runtime polling, input reading, and process inspection.
	const reserve = 16
	// At peak, Start can need six stdio pipe ends, two exec-error pipe ends,
	// a process handle, and a spill file. Each earlier attempt can retain one
	// more spill file; immediate output does not keep those files.
	perJob := uint64(10)
	if !opts.ImmediateOutput && opts.Retries > 0 {
		perJob += uint64(opts.Retries)
	}
	if softLimit <= opened+reserve || (softLimit-opened-reserve)/perJob == 0 {
		return 0, fmt.Errorf("open-file limit (%d) is too low for one job and its buffered retries; raise ulimit -n or reduce --retries", softLimit)
	}
	jobs := (softLimit - opened - reserve) / perJob
	if jobs >= uint64(opts.Jobs) {
		return opts.Jobs, nil
	}
	Log.Warningf("reducing concurrent jobs from %d to %d for open-file limit %d", opts.Jobs, jobs, softLimit)
	return int(jobs), nil
}
