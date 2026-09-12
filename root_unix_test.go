//go:build linux

package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestStopOnErrorExits(t *testing.T) {
	cmd := rushTestCommand(t,
		strings.Repeat("x\n", 8),
		"-j", "2", "-t", "1", "-e", "--cleanup-time", "0", "sleep", "30",
	)
	waitForRush(t, cmd, 5*time.Second)
	stderr := cmd.Stderr.(*lockedBuffer).String()
	if !strings.Contains(stderr, "stop on first error") {
		t.Fatalf("missing stop-on-error message:\n%s", stderr)
	}
	if strings.Contains(stderr, "cmd #3") {
		t.Fatalf("started another command after cancellation:\n%s", stderr)
	}
}

func TestStopOnExitErrorExits(t *testing.T) {
	cmd := rushTestCommand(t,
		"false\nsleep 30\nsleep 30\n",
		"-j", "2", "-e", "--cleanup-time", "0", "{}",
	)
	waitForRush(t, cmd, 5*time.Second)
	if code := cmd.ProcessState.ExitCode(); code != 1 {
		t.Fatalf("exit code: %d; want 1\nstderr:\n%s", code, cmd.Stderr)
	}
	if stderr := cmd.Stderr.(*lockedBuffer).String(); strings.Contains(stderr, "cmd #3") {
		t.Fatalf("started another command after cancellation:\n%s", stderr)
	}
}

func TestInterruptExits(t *testing.T) {
	readyFile := t.TempDir() + "/ready"
	cmd := rushTestCommand(t,
		"x\n",
		"-j", "1", "--cleanup-time", "0", fmt.Sprintf("echo ready > %q; sleep 30", readyFile),
	)

	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, readyFile, 2*time.Second)
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		cmd.Process.Kill()
		t.Fatalf("send interrupt: %v", err)
	}
	waitForStartedRush(t, cmd, 5*time.Second)
	if code := cmd.ProcessState.ExitCode(); code != 130 {
		t.Fatalf("exit code: %d; want 130\nstderr:\n%s", code, cmd.Stderr)
	}
	if stderr := cmd.Stderr.(*lockedBuffer).String(); !strings.Contains(stderr, "received an interrupt") {
		t.Fatalf("missing interrupt message:\n%s", stderr)
	}
}

func TestInterruptKillsChildAndSkipsQueuedCommand(t *testing.T) {
	testDir := t.TempDir()
	pidFile := testDir + "/child.pid"
	queuedFile := testDir + "/queued"
	longCmd := fmt.Sprintf("trap '' HUP INT TERM; sleep 30 & echo $! > %q; wait", pidFile)
	queuedCmd := fmt.Sprintf("echo started > %q", queuedFile)
	cmd := rushTestCommand(t, longCmd+"\n"+queuedCmd+"\n", "-j", "1", "--cleanup-time", "0", "{}")

	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := waitForPIDFile(t, pidFile, 2*time.Second)
	t.Cleanup(func() {
		syscall.Kill(pid, syscall.SIGKILL)
	})
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		cmd.Process.Kill()
		t.Fatalf("send interrupt: %v", err)
	}
	waitForStartedRush(t, cmd, 5*time.Second)

	assertProcessStopped(t, pid, time.Second)
	if _, err := os.Stat(queuedFile); !os.IsNotExist(err) {
		t.Fatalf("queued command ran after interrupt: %v", err)
	}
}

func TestInterruptAllowsGracefulCleanup(t *testing.T) {
	testDir := t.TempDir()
	readyFile := testDir + "/ready"
	cleanupFile := testDir + "/cleaned"
	cmdStr := fmt.Sprintf(
		"trap 'echo cleaned > %q; exit 0' INT; echo ready > %q; while :; do sleep 30; done",
		cleanupFile, readyFile,
	)
	cmd := rushTestCommand(t, "x\n", "-j", "1", "--cleanup-time", "2", cmdStr)

	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, readyFile, 2*time.Second)
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		cmd.Process.Kill()
		t.Fatalf("send interrupt: %v", err)
	}
	waitForStartedRush(t, cmd, 5*time.Second)
	if data, err := os.ReadFile(cleanupFile); err != nil || strings.TrimSpace(string(data)) != "cleaned" {
		t.Fatalf("graceful cleanup was not completed: data=%q err=%v", data, err)
	}
}

func TestSecondInterruptForcesExit(t *testing.T) {
	pidFile := t.TempDir() + "/child.pid"
	cmdStr := fmt.Sprintf("trap '' HUP INT TERM; sleep 3 & echo $! > %q; wait", pidFile)
	cmd := rushTestCommand(t, "x\n", "-j", "1", "--cleanup-time", "30", cmdStr)

	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := waitForPIDFile(t, pidFile, 2*time.Second)
	t.Cleanup(func() {
		syscall.Kill(pid, syscall.SIGKILL)
	})
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	waitForBufferContains(t, cmd.Stderr.(*lockedBuffer), "received an interrupt", 2*time.Second)
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	secondInterrupt := time.Now()
	waitForStartedRush(t, cmd, 5*time.Second)
	if elapsed := time.Since(secondInterrupt); elapsed > time.Second {
		t.Fatalf("second interrupt took %s to force exit", elapsed)
	}
	assertProcessStopped(t, pid, time.Second)
}

func TestStopOnErrorKillsSiblingAndSkipsQueuedCommand(t *testing.T) {
	testDir := t.TempDir()
	pidFile := testDir + "/child.pid"
	queuedFile := testDir + "/queued"
	longCmd := fmt.Sprintf("trap '' HUP INT TERM; sleep 30 & echo $! > %q; wait", pidFile)
	failingCmd := fmt.Sprintf("while [ ! -s %q ]; do sleep 0.01; done; false", pidFile)
	queuedCmd := fmt.Sprintf("echo started > %q", queuedFile)
	input := strings.Join([]string{failingCmd, longCmd, queuedCmd, ""}, "\n")
	cmd := rushTestCommand(t, input, "-j", "2", "-e", "--cleanup-time", "0", "{}")
	waitForRush(t, cmd, 5*time.Second)

	pid := waitForPIDFile(t, pidFile, time.Second)
	t.Cleanup(func() {
		syscall.Kill(pid, syscall.SIGKILL)
	})
	assertProcessStopped(t, pid, time.Second)
	if _, err := os.Stat(queuedFile); !os.IsNotExist(err) {
		t.Fatalf("queued command ran after stop-on-error: %v", err)
	}
}

func TestSIGTERMExits143(t *testing.T) {
	readyFile := t.TempDir() + "/ready"
	cmd := rushTestCommand(t, "x\n", "--cleanup-time", "0", fmt.Sprintf("echo ready > %q; sleep 30", readyFile))
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, readyFile, 2*time.Second)
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	waitForStartedRush(t, cmd, 5*time.Second)
	if code := cmd.ProcessState.ExitCode(); code != 143 {
		t.Fatalf("exit code: %d; want 143\nstderr:\n%s", code, cmd.Stderr)
	}
}

func TestSignalInterruptsBlockedFIFOInput(t *testing.T) {
	fifo := t.TempDir() + "/input.fifo"
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	writerReady := make(chan int, 1)
	go func() {
		fd, _ := syscall.Open(fifo, syscall.O_WRONLY, 0)
		writerReady <- fd
	}()
	cmd := rushTestCommand(t, "", "-i", fifo, "echo", "{}")
	cmd.Stdin = nil
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var writerFD int
	select {
	case writerFD = <-writerReady:
		if writerFD < 0 {
			t.Fatal("open FIFO writer failed")
		}
		defer syscall.Close(writerFD)
	case <-time.After(2 * time.Second):
		t.Fatal("rush did not open FIFO reader")
	}
	waitForOpenPath(t, cmd.Process.Pid, fifo, 2*time.Second)
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	waitForStartedRush(t, cmd, 5*time.Second)
	if code := cmd.ProcessState.ExitCode(); code != 130 {
		t.Fatalf("exit code: %d; want 130\nstderr:\n%s", code, cmd.Stderr)
	}
}

func TestSignalInterruptsRetryWait(t *testing.T) {
	cmd := rushTestCommand(t, "x\n", "-r", "2", "--retry-interval", "30", "false")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitForBufferContains(t, cmd.Stderr.(*lockedBuffer), "wait cmd", 2*time.Second)
	started := time.Now()
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	waitForStartedRush(t, cmd, 5*time.Second)
	if code := cmd.ProcessState.ExitCode(); code != 130 {
		t.Fatalf("exit code: %d; want 130", code)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("retry cancellation took %s", elapsed)
	}
}

func waitForBufferContains(t *testing.T, buf *lockedBuffer, needle string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Contains(buf.String(), needle) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("output did not contain %q within %s:\n%s", needle, timeout, buf.String())
}

func waitForOpenPath(t *testing.T, pid int, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	fdDir := fmt.Sprintf("/proc/%d/fd", pid)
	for time.Now().Before(deadline) {
		entries, _ := os.ReadDir(fdDir)
		for _, entry := range entries {
			target, _ := os.Readlink(fdDir + "/" + entry.Name())
			if target == path {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("process %d did not open %s within %s", pid, path, timeout)
}

func TestTimeoutKillsChildProcess(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
	}{
		{name: "buffered output"},
		{name: "immediate output", args: []string{"-I"}},
		{name: "keep order", args: []string{"-k"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			pidFile := t.TempDir() + "/child.pid"
			cmdStr := fmt.Sprintf("trap '' HUP INT TERM; sleep 30 & echo $! > %q; wait", pidFile)
			input := cmdStr + "\nsleep 0.2; echo unaffected\n"
			args := append([]string{"-j", "2", "-t", "1"}, tt.args...)
			args = append(args, "{}")
			cmd := rushTestCommand(t, input, args...)
			waitForRush(t, cmd, 5*time.Second)
			if stdout := cmd.Stdout.(*lockedBuffer).String(); !strings.Contains(stdout, "unaffected") {
				t.Fatalf("another command was interrupted by the timeout:\n%s", stdout)
			}

			pid := waitForPIDFile(t, pidFile, time.Second)
			t.Cleanup(func() {
				syscall.Kill(pid, syscall.SIGKILL)
			})
			assertProcessStopped(t, pid, time.Second)
		})
	}
}

func TestTimeoutIs124WithoutChildStatusPropagation(t *testing.T) {
	cmd := rushTestCommand(t, "x\n", "-t", "1", "--propagate-exit-status=false", "sleep", "30")
	waitForRush(t, cmd, 5*time.Second)
	if code := cmd.ProcessState.ExitCode(); code != 124 {
		t.Fatalf("exit code: %d; want 124\nstderr:\n%s", code, cmd.Stderr)
	}
}

func TestSuccessfulCommandFileRollsBackOnOutputFailure(t *testing.T) {
	successFile := t.TempDir() + "/successful.rush"
	original := []byte("already complete__CMD__\n")
	if err := os.WriteFile(successFile, original, 0600); err != nil {
		t.Fatal(err)
	}
	cmd := rushTestCommand(t, "x\n", "-c", "-C", successFile, "-o", "/dev/full", "printf", "output")
	waitForRush(t, cmd, 5*time.Second)
	if code := cmd.ProcessState.ExitCode(); code != 1 {
		t.Fatalf("exit code: %d; want 1\nstderr:\n%s", code, cmd.Stderr)
	}
	got, err := os.ReadFile(successFile)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("successful-command file = %q; want original %q", got, original)
	}
}

func TestCleanupCatchesDescendantCreatedBySignalHandler(t *testing.T) {
	dir := t.TempDir()
	ready := dir + "/ready"
	dynamic := dir + "/dynamic.pid"
	command := fmt.Sprintf("trap 'sleep 30 & echo $! > %q; while :; do sleep 30; done' INT; echo ready > %q; while :; do sleep 30; done", dynamic, ready)
	cmd := rushTestCommand(t, "x\n", "--cleanup-time", "1", command)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, ready, 2*time.Second)
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	waitForStartedRush(t, cmd, 5*time.Second)
	pid := waitForPIDFile(t, dynamic, time.Second)
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	assertProcessStopped(t, pid, time.Second)
}

func waitForPIDFile(t *testing.T, path string, timeout time.Duration) int {
	t.Helper()
	waitForFile(t, path, timeout)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("parse child pid: %v", err)
	}
	return pid
}

func waitForFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		_, err := os.Stat(path)
		if err == nil {
			return
		}
		if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("file %s was not created within %s", path, timeout)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func assertProcessStopped(t *testing.T, pid int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for processRunning(pid) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if processRunning(pid) {
		t.Fatalf("child process %d is still running", pid)
	}
}

func processRunning(pid int) bool {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	fields := strings.Fields(string(data))
	return len(fields) < 3 || fields[2] != "Z"
}

func rushTestCommand(t *testing.T, stdin string, args ...string) *exec.Cmd {
	t.Helper()
	testArgs := append([]string{"-test.run=^TestRushHelperProcess$", "--"}, args...)
	cmd := exec.Command(os.Args[0], testArgs...)
	cmd.Env = append(os.Environ(), "RUSH_TEST_HELPER_PROCESS=1")
	cmd.Stdin = strings.NewReader(stdin)
	cmd.Stdout = &lockedBuffer{}
	cmd.Stderr = &lockedBuffer{}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func waitForRush(t *testing.T, cmd *exec.Cmd, timeout time.Duration) {
	t.Helper()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitForStartedRush(t, cmd, timeout)
}

func waitForStartedRush(t *testing.T, cmd *exec.Cmd, timeout time.Duration) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	select {
	case <-done:
		return
	case <-time.After(timeout):
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		t.Fatalf("rush did not exit within %s\nstderr:\n%s", timeout, fmt.Sprint(cmd.Stderr))
	}
}
