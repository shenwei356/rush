//go:build windows

package process

// Windows process handles are not governed by Unix's RLIMIT_NOFILE.
func fileDescriptorJobLimit(opts *Options) (int, error) { return opts.Jobs, nil }
