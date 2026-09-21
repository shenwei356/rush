//go:build !windows

package process

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestUnixCleanupAfterGroupLeaderExited(t *testing.T) {
	pidFile := t.TempDir() + "/child.pid"
	opts := &Options{CleanupTime: 0}
	controller, err := newPlatformProcessController(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer controller.Close()
	cmd := getCommand(context.Background(), fmt.Sprintf("sh -c 'trap \"\" INT; exec sleep 30' & echo $! > %q", pidFile))
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }()
	if err := controller.Started(cmd); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatal(err)
	}
	defer controller.Finished(cmd)
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	pid := waitProcessPIDFile(t, pidFile)
	defer func() { _ = syscall.Kill(pid, syscall.SIGKILL) }()
	if err := controller.KillCommand(cmd); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err == syscall.ESRCH {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("command descendant survived after the group leader exited")
}

func waitProcessPIDFile(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
			if err != nil {
				t.Fatal(err)
			}
			return pid
		}
		if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("child PID was not recorded")
	return 0
}

type failingStopController struct {
	processController
	err     error
	started chan *exec.Cmd
}

func (c *failingStopController) Started(cmd *exec.Cmd) error {
	err := c.processController.Started(cmd)
	if err == nil {
		c.started <- cmd
	}
	return err
}
func (c *failingStopController) KillCommand(*exec.Cmd) error { return c.err }

func TestStopFailureKillsDirectChildAndReportsError(t *testing.T) {
	sentinel := errors.New("stop failed")
	opts := &Options{CleanupTime: 0}
	inner, err := newPlatformProcessController(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer inner.Close()
	controller := &failingStopController{processController: inner, err: sentinel, started: make(chan *exec.Cmd, 1)}
	c := NewCommand(1, "exec sleep 30", make(chan struct{}), 50*time.Millisecond)
	c.controller = controller
	done := make(chan error, 1)
	go func() {
		out, err := c.Run(opts, 1)
		for range out {
		}
		done <- err
	}()
	cmd := <-controller.started
	defer func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }()
	select {
	case err := <-done:
		if !errors.Is(err, sentinel) {
			t.Fatalf("Run error=%v; want stop failure", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run waited for a child after stopping it failed")
	}
}

func TestUnixGroupSignalPermissionFallsBackToMembers(t *testing.T) {
	old := sendUnixGroupSignal
	sendUnixGroupSignal = func(int, syscall.Signal) error { return syscall.EPERM }
	t.Cleanup(func() { sendUnixGroupSignal = old })

	opts := &Options{CleanupTime: 0}
	controller, err := newPlatformProcessController(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer controller.Close()
	pidFile := t.TempDir() + "/child.pid"
	cmd := getCommand(context.Background(), fmt.Sprintf("sh -c 'trap \"\" INT; exec sleep 30' & echo $! > %q; wait", pidFile))
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }()
	if err := controller.Started(cmd); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatal(err)
	}
	defer controller.Finished(cmd)
	childPID := waitProcessPIDFile(t, pidFile)
	defer func() { _ = syscall.Kill(childPID, syscall.SIGKILL) }()
	if err := controller.KillCommand(cmd); err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	select {
	case <-waited:
	case <-time.After(3 * time.Second):
		t.Fatal("group signal fallback left a command running")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		child, err := lookupPlatformProcess(childPID)
		if errors.Is(err, os.ErrNotExist) || (err == nil && child.zombie) {
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("group signal fallback left a child running")
}

func TestStopFailureDoesNotWaitForeverForDescendantOutput(t *testing.T) {
	sentinel := errors.New("stop failed")
	opts := &Options{CleanupTime: 0}
	inner, err := newPlatformProcessController(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer inner.Close()
	controller := &failingStopController{processController: inner, err: sentinel, started: make(chan *exec.Cmd, 1)}
	c := NewCommand(1, "sh -c 'trap \"\" INT; exec sleep 30' & wait", make(chan struct{}), 50*time.Millisecond)
	c.controller = controller
	done := make(chan error, 1)
	go func() {
		out, err := c.Run(opts, 1)
		for range out {
		}
		done <- err
	}()
	cmd := <-controller.started
	defer func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }()
	select {
	case err := <-done:
		if !errors.Is(err, sentinel) {
			t.Fatalf("Run error=%v; want stop failure", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("Run kept waiting for a descendant's output pipe")
	}
}
