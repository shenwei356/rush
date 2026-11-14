//go:build darwin || freebsd || openbsd

package process

import (
	"context"
	"fmt"
	"os"
)

// Exists reports whether a visible live process has a matching name,
// excluding the calling process and zombies.
func (m *NameMatcher) Exists(ctx context.Context) (bool, error) {
	all, err := snapshotPlatformProcesses()
	if err != nil {
		return false, err
	}
	for _, p := range all {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if p.pid == os.Getpid() || p.zombie || !m.mayMatchPrefix(p.name) {
			continue
		}
		fullName, err := fullPlatformProcessName(p)
		if unixProcessGone(err) {
			continue
		}
		if err != nil {
			return false, fmt.Errorf("read name of pid %d: %w", p.pid, err)
		}
		if m.matches(fullName) {
			return true, nil
		}
	}
	return false, nil
}
