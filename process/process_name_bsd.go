//go:build freebsd || openbsd

package process

func fullPlatformProcessName(p platformProcess) (string, error) { return p.name, nil }
