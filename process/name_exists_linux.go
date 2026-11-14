//go:build linux

package process

import (
	"context"
	"fmt"
	"os"
	"strconv"
)

// Exists reports whether a visible live process has a matching name.
// The calling process and zombies are excluded. Inspection errors are returned
// rather than treated as absence, which could prematurely release waiting jobs.
func (m *NameMatcher) Exists(ctx context.Context) (bool, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 || pid == os.Getpid() {
			continue
		}
		p, err := readLinuxProcess(pid)
		if unixProcessGone(err) {
			continue
		}
		if err != nil {
			return false, fmt.Errorf("inspect pid %d: %w", pid, err)
		}
		if p.zombie {
			continue
		}
		// Only resolve a truncated name when it could match the requested name.
		if len(p.name) >= 15 && m.mayMatchPrefix(p.name) {
			p.name, err = fullPlatformProcessName(p)
			if unixProcessGone(err) {
				continue
			}
			if err != nil {
				return false, fmt.Errorf("read name of pid %d: %w", pid, err)
			}
		}
		if m.matches(p.name) {
			return true, nil
		}
	}
	return false, nil
}
