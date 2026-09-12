//go:build !windows

package main

import (
	"context"
	"errors"
	"io"
	"os"
	"syscall"
	"time"
)

func openInputFile(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

type cancellableInputReader struct {
	file *os.File
	ctx  context.Context
}

func newInputReader(file *os.File, ctx context.Context) io.Reader {
	_ = syscall.SetNonblock(int(file.Fd()), true)
	return &cancellableInputReader{file: file, ctx: ctx}
}
func (r *cancellableInputReader) Read(p []byte) (int, error) {
	for {
		n, err := r.file.Read(p)
		if !errors.Is(err, syscall.EAGAIN) && !errors.Is(err, syscall.EWOULDBLOCK) {
			return n, err
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-r.ctx.Done():
			timer.Stop()
			return 0, r.ctx.Err()
		case <-timer.C:
		}
	}
}

func terminationSignals() []os.Signal { return []os.Signal{os.Interrupt, syscall.SIGTERM} }
func terminationStatus(sig os.Signal) int {
	if sig == syscall.SIGTERM {
		return 143
	}
	return 130
}
