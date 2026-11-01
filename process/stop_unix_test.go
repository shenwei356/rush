//go:build !windows

package process

import (
	"bufio"
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

func TestStandaloneSpillWriteFailureStopsLiveCommand(t *testing.T) {
	withSpillSeams(t)
	oldLimit := TmpOutputDataBuffer
	TmpOutputDataBuffer = 0
	t.Cleanup(func() { TmpOutputDataBuffer = oldLimit })
	sentinel := errors.New("spill write failed")
	writeSpillFile = func(*bufio.Writer, []byte) (int, error) { return 0, sentinel }
	dir := t.TempDir()
	createSpillFile = func() (*os.File, error) { return os.CreateTemp(dir, "spill-") }
	pidFile := dir + "/pid"
	opts := &Options{OutFileHandle: os.Stdout, ErrFileHandle: os.Stderr}
	// No scheduler state or timeout: the copier failure itself must stop this
	// otherwise long-running process and complete Command.Run.
	c := NewCommand(1, fmt.Sprintf("echo $$ > %q; printf output; exec sleep 30", pidFile), nil, 0)
	done := make(chan error, 1)
	go func() {
		out, err := c.Run(opts, 1)
		for range out {
		}
		done <- err
	}()
	pid := waitProcessPIDFile(t, pidFile)
	finished := false
	t.Cleanup(func() {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		if !finished {
			<-done
		}
		_ = c.Cleanup()
	})
	select {
	case err := <-done:
		finished = true
		if err == nil || !errors.Is(c.outputErr, sentinel) {
			t.Fatalf("output failure was lost: err=%v outputErr=%v", err, c.outputErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("output failure waited for the command's natural exit")
	}
	if p, err := lookupPlatformProcess(pid); !unixProcessGone(err) && (err != nil || !p.zombie) {
		t.Fatalf("command survived output failure: process=%+v err=%v", p, err)
	}
	if err := c.Cleanup(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "pid" {
		t.Fatalf("spill files left behind: %v, err=%v", entries, err)
	}
}

// waitFailureController shortens only pipe-drain waiting in these tests and
// records cleanup ordering while retaining the real platform controller.
type waitFailureController struct {
	processController
	events  []string
	groups  []int
	stopErr error // Optional fault reported after the real descendant cleanup.
}

func (c *waitFailureController) Started(cmd *exec.Cmd) error {
	cmd.WaitDelay = 50 * time.Millisecond
	c.events = append(c.events, "start")
	c.groups = append(c.groups, cmd.Process.Pid)
	return c.processController.Started(cmd)
}

func (c *waitFailureController) KillCommand(cmd *exec.Cmd) error {
	c.events = append(c.events, "stop")
	return errors.Join(c.processController.KillCommand(cmd), c.stopErr)
}

func TestWaitFailureCleansDescendantsBeforeRetry(t *testing.T) {
	// Wait returns ExitError instead of ErrWaitDelay when the shell also fails.
	// Both cases must clean up surviving descendants before another attempt.
	for _, tc := range []struct {
		status    int
		stopFails bool
		stdin     bool
	}{{0, false, false}, {7, false, false}, {0, true, false}, {0, false, true}, {7, false, true}, {0, true, true}} {
		t.Run(fmt.Sprintf("shell-status=%d/stop-fails=%t/stdin=%t", tc.status, tc.stopFails, tc.stdin), func(t *testing.T) {
			old := makeProcessController
			controller := &waitFailureController{}
			if tc.stopFails {
				controller.stopErr = errors.New("descendant cleanup failed")
			}
			makeProcessController = func(opts *Options) (processController, error) {
				inner, err := newPlatformProcessController(opts)
				controller.processController = inner
				return controller, err
			}
			t.Cleanup(func() {
				makeProcessController = old
				for _, group := range controller.groups {
					_ = syscall.Kill(-group, syscall.SIGKILL)
				}
			})
			pidFile := t.TempDir() + "/children"
			opts := &Options{Jobs: 1, Retries: 1, PropExitStatus: true, CleanupTime: 0}
			job := Job{Cmd: fmt.Sprintf("sleep 30 & echo $! >> %q; exit %d", pidFile, tc.status)}
			if tc.stdin {
				// Keep only stdin open in the descendant. A payload larger than
				// pipe capacity must not leave the parent's input copier stuck.
				job.Cmd = fmt.Sprintf("exec 3<&0; sleep 30 <&3 3<&- >/dev/null 2>&1 & echo $! >> %q; exec 3<&-; exit %d", pidFile, tc.status)
				job.Stdin = strings.Repeat("x", 1<<20)
			}
			got := observeRun(t, opts, job)
			wantEvents, wantAttempts, wantCause := "start,stop,start,stop", 2, 0
			if tc.stopFails {
				wantEvents, wantAttempts, wantCause = "start,stop", 1, 1
				if len(got.commands) != 1 || !errors.Is(got.commands[0].Err, controller.stopErr) {
					t.Fatal("descendant cleanup error was lost")
				}
			}
			if events := strings.Join(controller.events, ","); events != wantEvents {
				t.Errorf("lifecycle=%s; want cleanup before retry and after final failure", events)
			}
			wantStatus := tc.status
			if wantStatus == 0 {
				wantStatus = 1
			}
			if len(got.statuses) != 1 || got.statuses[0] != wantStatus || got.cause.Status != wantCause {
				t.Fatalf("statuses=%v, cause=%v; want child status %d", got.statuses, got.cause, wantStatus)
			}
			if tc.status == 0 && (len(got.commands) != 1 || !errors.Is(got.commands[0].Err, exec.ErrWaitDelay)) {
				t.Fatal("pipe-drain error was lost")
			}
			data, err := os.ReadFile(pidFile)
			if err != nil {
				t.Fatal(err)
			}
			pids := strings.Fields(string(data))
			if len(pids) != wantAttempts {
				t.Fatalf("attempts=%d; want %d", len(pids), wantAttempts)
			}
			for _, text := range pids {
				pid, err := strconv.Atoi(text)
				if err != nil {
					t.Fatal(err)
				}
				deadline := time.Now().Add(time.Second)
				for {
					child, err := lookupPlatformProcess(pid)
					if unixProcessGone(err) || (err == nil && child.zombie) {
						break
					}
					if err != nil || time.Now().After(deadline) {
						t.Fatalf("child %d survived cleanup: %v", pid, err)
					}
					time.Sleep(10 * time.Millisecond)
				}
			}
		})
	}
}

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

func TestFallbackExclusionsRequireCurrentName(t *testing.T) {
	// No process can have this PID. Exercise an unavailable name without
	// signaling any process or relying on a PID-reuse timing window.
	cmd := &exec.Cmd{Path: "sh", Process: &os.Process{Pid: -1}}
	for _, test := range []struct {
		name     string
		excluded []string
		allowed  bool
		wantErr  bool
	}{
		{"none", nil, true, false},
		{"empty", []string{""}, true, false},
		{"all", []string{"all"}, false, false},
		{"specific", []string{"sleep"}, false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			allowed, err := canKillOwnedCommand(cmd, test.excluded)
			if allowed != test.allowed || (err != nil) != test.wantErr {
				t.Fatalf("allowed=%t err=%v; want allowed=%t error=%t", allowed, err, test.allowed, test.wantErr)
			}
		})
	}
}

func TestStopFailureHonorsExecutedProgramExclusion(t *testing.T) {
	for _, excluded := range []string{"sleep", "unrelated-program"} {
		t.Run(excluded, func(t *testing.T) {
			sentinel := errors.New("process-tree inspection failed")
			opts := &Options{NoKillExes: []string{excluded}, OutFileHandle: os.Stdout, ErrFileHandle: os.Stderr}
			inner, err := newPlatformProcessController(opts)
			if err != nil {
				t.Fatal(err)
			}
			defer inner.Close()
			controller := &failingStopController{processController: inner, err: sentinel, started: make(chan *exec.Cmd, 1)}
			cancel := make(chan struct{})
			c := NewCommand(1, "exec sleep 30", cancel, 0)
			c.controller = controller
			done := make(chan error, 1)
			go func() {
				out, err := c.Run(opts, 1)
				for range out {
				}
				done <- err
			}()
			var cmd *exec.Cmd
			select {
			case cmd = <-controller.started:
			case err := <-done:
				t.Fatalf("command did not start: %v", err)
			case <-time.After(2 * time.Second):
				t.Fatal("command did not register")
			}
			finished := false
			t.Cleanup(func() {
				_ = cmd.Process.Kill()
				if !finished {
					<-done
				}
				_ = c.Cleanup()
			})
			// The shell path remains "sh" after exec replaces it with sleep.
			// Wait for that replacement before triggering the fallback path.
			deadline := time.Now().Add(2 * time.Second)
			for {
				p, err := lookupPlatformProcess(cmd.Process.Pid)
				if err == nil && p.name == "sleep" {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("shell did not exec sleep: process=%+v err=%v", p, err)
				}
				time.Sleep(time.Millisecond)
			}
			close(cancel)
			if excluded == "sleep" {
				select {
				case err := <-done:
					finished = true
					t.Fatalf("fallback killed the excluded executable: %v", err)
				case <-time.After(100 * time.Millisecond):
				}
				// End the owned helper explicitly so the test does not leave an
				// intentionally excluded process waiting for its natural exit.
				_ = cmd.Process.Kill()
			}
			select {
			case err := <-done:
				finished = true
				if !errors.Is(err, sentinel) || !errors.Is(err, ErrCancelled) {
					t.Fatalf("cleanup error lost: %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("fallback did not finish cleanup")
			}
		})
	}
}

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
		if unixProcessGone(err) || (err == nil && child.zombie) {
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

func TestUnixCleanupContinuesAfterMemberSignalFailure(t *testing.T) {
	for _, filtered := range []bool{false, true} {
		t.Run(fmt.Sprintf("filtered=%t", filtered), func(t *testing.T) {
			oldGroup, oldProcess := sendUnixGroupSignal, sendUnixProcessSignal
			t.Cleanup(func() { sendUnixGroupSignal, sendUnixProcessSignal = oldGroup, oldProcess })
			opts := &Options{CleanupTime: 0}
			if filtered {
				opts.NoStopExes = []string{"not-a-running-program"}
				opts.NoKillExes = []string{"not-a-running-program"}
			} else {
				// Exercise the existing fallback when signaling a whole group is denied.
				sendUnixGroupSignal = func(int, syscall.Signal) error { return syscall.EPERM }
			}
			controller, err := newPlatformProcessController(opts)
			if err != nil {
				t.Fatal(err)
			}
			defer controller.Close()
			dir := t.TempDir()
			one, two := dir+"/one.pid", dir+"/two.pid"
			command := fmt.Sprintf("trap '' INT; sleep 30 & echo $! > %q; sleep 30 & echo $! > %q; wait", one, two)
			cmd := getCommand(context.Background(), command)
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); _ = cmd.Wait() })
			if err := controller.Started(cmd); err != nil {
				t.Fatal(err)
			}
			defer controller.Finished(cmd)
			first, second := waitProcessPIDFile(t, one), waitProcessPIDFile(t, two)
			denied, allowed := max(first, second), min(first, second)
			attempted := make(map[syscall.Signal]map[int]bool)
			sendUnixProcessSignal = func(pid int, sig syscall.Signal) error {
				if attempted[sig] == nil {
					attempted[sig] = make(map[int]bool)
				}
				attempted[sig][pid] = true
				if pid == denied {
					return syscall.EPERM
				}
				return oldProcess(pid, sig)
			}
			if err := controller.KillCommand(cmd); !errors.Is(err, syscall.EPERM) {
				t.Errorf("cleanup error=%v; want permission failure", err)
			}
			for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGKILL} {
				if !attempted[sig][allowed] {
					t.Errorf("signal %v was not attempted for eligible child %d", sig, allowed)
				}
			}
			if child, err := lookupPlatformProcess(denied); err != nil || child.zombie {
				t.Fatalf("cleanup bypassed the denied signal: process=%+v err=%v", child, err)
			}
			deadline := time.Now().Add(time.Second)
			for {
				p, err := lookupPlatformProcess(allowed)
				if unixProcessGone(err) || (err == nil && p.zombie) {
					break
				}
				if err != nil || time.Now().After(deadline) {
					t.Fatalf("eligible child survived cleanup: %v", err)
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
}
