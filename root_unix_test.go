//go:build linux

package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRushHelperProcess(t *testing.T) {
	if os.Getenv("RUSH_TEST_HELPER_PROCESS") != "1" {
		return
	}

	for i, arg := range os.Args {
		if arg == "--" {
			RootCmd.SetArgs(os.Args[i+1:])
			Execute()
			return
		}
	}
	os.Exit(2)
}

func TestStopOnErrorExits(t *testing.T) {
	cmd := rushTestCommand(t,
		strings.Repeat("x\n", 8),
		"-j", "2", "-t", "1", "-e", "--cleanup-time", "0", "sleep", "30",
	)
	waitForRush(t, cmd, 5*time.Second)
	stderr := cmd.Stderr.(*bytes.Buffer).String()
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
	if stderr := cmd.Stderr.(*bytes.Buffer).String(); strings.Contains(stderr, "cmd #3") {
		t.Fatalf("started another command after cancellation:\n%s", stderr)
	}
}

func TestInterruptExits(t *testing.T) {
	cmd := rushTestCommand(t,
		strings.Repeat("x\n", 4),
		"-j", "2", "--cleanup-time", "0", "sleep", "30",
	)

	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(250 * time.Millisecond)
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		cmd.Process.Kill()
		t.Fatalf("send interrupt: %v", err)
	}
	waitForStartedRush(t, cmd, 5*time.Second)
	if stderr := cmd.Stderr.(*bytes.Buffer).String(); !strings.Contains(stderr, "received an interrupt") {
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
	time.Sleep(200 * time.Millisecond)
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
	failingCmd := "sleep 0.5; false"
	longCmd := fmt.Sprintf("trap '' HUP INT TERM; sleep 30 & echo $! > %q; wait", pidFile)
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
			if stdout := cmd.Stdout.(*bytes.Buffer).String(); !strings.Contains(stdout, "unaffected") {
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
	cmd.Stdout = &bytes.Buffer{}
	cmd.Stderr = &bytes.Buffer{}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd
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
