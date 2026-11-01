//go:build !windows

package main

import (
	"context"
	"errors"
	"io"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// openInputFile opens owned inputs without waiting for a FIFO writer and
// excludes the descriptor from child processes.
func openInputFile(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

// newInterruptibleOutput enables write deadlines on inherited pipes and sockets.
// The returned cleanup restores shared descriptor flags and closes only our copy.
func newInterruptibleOutput(file *os.File) (*os.File, func() error, error) {
	noop := func() error { return nil }
	info, err := file.Stat()
	if err != nil {
		return nil, nil, err
	}
	if info.Mode()&(os.ModeNamedPipe|os.ModeSocket) == 0 {
		return file, noop, nil
	}
	// Even pollable stdout/stderr need a private descriptor: Go may terminate
	// on EPIPE from descriptors 1 and 2, bypassing child cleanup. A copy also
	// keeps deadlines independent of the original file's lifetime.
	raw, err := file.SyscallConn()
	if err != nil {
		return nil, nil, err
	}
	fd := -1
	changedFlags := false
	var setupErr error
	err = raw.Control(func(original uintptr) {
		var flags int
		flags, setupErr = unix.FcntlInt(original, unix.F_GETFL, 0)
		if setupErr != nil {
			return
		}
		fd, setupErr = unix.FcntlInt(original, unix.F_DUPFD_CLOEXEC, 3)
		if setupErr == nil && flags&syscall.O_NONBLOCK == 0 {
			setupErr = syscall.SetNonblock(fd, true)
			changedFlags = setupErr == nil
		}
	})
	if err != nil || setupErr != nil {
		if fd >= 0 {
			_ = syscall.Close(fd)
		}
		return nil, nil, errors.Join(err, setupErr)
	}
	// NewFile observes O_NONBLOCK and registers the copy with Go's poller.
	output := os.NewFile(uintptr(fd), file.Name())
	closed := false
	cleanup := func() error {
		if closed {
			return nil
		}
		closed = true
		var restoreErr error
		if changedFlags {
			restoreErr = syscall.SetNonblock(fd, false)
		}
		return errors.Join(restoreErr, output.Close())
	}
	if err := output.SetWriteDeadline(time.Time{}); err != nil {
		return nil, nil, errors.Join(err, cleanup())
	}
	return output, cleanup, nil
}

type cancellableInputReader struct {
	file         *os.File
	ctx          context.Context
	waitReadable bool // Pipes must become readable before an initial EOF is meaningful.
}

// newInputReader makes reads cancellable and returns cleanup that restores the
// inherited open-file description after reading has stopped, even on cancellation.
func newInputReader(file *os.File, ctx context.Context) (io.Reader, func() error, error) {
	info, err := file.Stat()
	if err != nil {
		return nil, nil, err
	}
	raw, err := file.SyscallConn()
	if err != nil {
		return nil, nil, err
	}
	// Cancellation can close file before cleanup runs. Keep a close-on-exec
	// duplicate only when changing its flags, so restoration still has a handle.
	restoreFD := -1
	var setupErr error
	err = raw.Control(func(fd uintptr) {
		var flags int
		flags, setupErr = unix.FcntlInt(fd, unix.F_GETFL, 0)
		if setupErr != nil || flags&syscall.O_NONBLOCK != 0 {
			return
		}
		restoreFD, setupErr = unix.FcntlInt(fd, unix.F_DUPFD_CLOEXEC, 0)
		if setupErr == nil {
			setupErr = syscall.SetNonblock(int(fd), true)
		}
	})
	if err != nil || setupErr != nil {
		if restoreFD >= 0 {
			_ = syscall.Close(restoreFD)
		}
		return nil, nil, errors.Join(err, setupErr)
	}
	restore := func() error {
		if restoreFD < 0 {
			return nil
		}
		err := syscall.SetNonblock(restoreFD, false)
		closeErr := syscall.Close(restoreFD)
		restoreFD = -1
		return errors.Join(err, closeErr)
	}
	return &cancellableInputReader{file: file, ctx: ctx, waitReadable: info.Mode()&os.ModeNamedPipe != 0}, restore, nil
}
func (r *cancellableInputReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		if err := r.ctx.Err(); err != nil {
			return 0, err
		}
		if r.waitReadable {
			ready, err := r.pipeReady()
			if err != nil {
				return 0, err
			}
			if !ready {
				continue
			}
			r.waitReadable = false
		}
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

// pipeReady waits for data or writer closure before the first read. A
// nonblocking FIFO read alone cannot distinguish no writer from end of input.
func (r *cancellableInputReader) pipeReady() (bool, error) {
	raw, err := r.file.SyscallConn()
	if err != nil {
		return false, err
	}
	var events int16
	var pollErr error
	err = raw.Control(func(fd uintptr) {
		poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		// Bound the wait so cancellation can close the file after Control
		// releases its descriptor reference.
		_, pollErr = unix.Poll(poll, 10)
		events = poll[0].Revents
	})
	if errors.Is(pollErr, syscall.EINTR) {
		pollErr = nil
	}
	if err != nil || pollErr != nil {
		return false, errors.Join(err, pollErr)
	}
	if events&unix.POLLNVAL != 0 {
		return false, syscall.EBADF
	}
	return events&(unix.POLLIN|unix.POLLHUP|unix.POLLERR) != 0, nil
}

func terminationSignals() []os.Signal { return []os.Signal{os.Interrupt, syscall.SIGTERM} }
func terminationStatus(sig os.Signal) int {
	if sig == syscall.SIGTERM {
		return 143
	}
	return 130
}
