//go:build !windows

package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestContinueJobIDsPreserveUnfinishedTasks(t *testing.T) {
	for _, test := range []struct {
		name, input, resumed, want string
		batch                      string
		successID                  int
	}{
		{"distinct", "a\nb\n", "a\nb\n", "2\n", "1", 1},
		{"reordered", "a\nb\n", "b\na\n", "1\n", "1", 1},
		{"duplicate", "a\na\n", "a\na\n", "2\n", "1", 1},
		{"later duplicate succeeded", "a\na\n", "a\na\n", "1\n", "1", 2},
		{"batches", "a\nb\nc\n", "a\nb\nc\n", "2\n", "2", 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			ready, journal := dir+"/ready", dir+"/records"
			command := fmt.Sprintf("if [ {#} -eq %d ] || [ -f %q ]; then echo {#}; else exit 7; fi", test.successID, ready)
			args := []string{"-j", "1", "-n", test.batch, "-c", "-C", journal, command}
			stdout, stderr, code := runLegacyRush(t, test.input, nil, args...)
			if code != 7 || stdout != fmt.Sprintf("%d\n", test.successID) {
				t.Fatalf("initial run: exit=%d stdout=%q stderr=%q", code, stdout, stderr)
			}
			if err := os.WriteFile(ready, nil, 0600); err != nil {
				t.Fatal(err)
			}
			stdout, stderr, code = runLegacyRush(t, test.resumed, nil, args...)
			if code != 0 || stdout != test.want {
				t.Fatalf("unfinished task lost or completed task rerun: exit=%d stdout=%q stderr=%q; want %q", code, stdout, stderr, test.want)
			}
			stdout, stderr, code = runLegacyRush(t, test.resumed, nil, args...)
			if code != 0 || stdout != "" {
				t.Fatalf("completed tasks rerun: exit=%d stdout=%q stderr=%q", code, stdout, stderr)
			}
		})
	}
}

func TestNamedPipeOutputLifecycle(t *testing.T) {
	for _, mode := range []string{"buffered", "immediate", "preview", "close-error"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			fifo, journal := dir+"/output", dir+"/records"
			if err := syscall.Mkfifo(fifo, 0600); err != nil {
				t.Fatal(err)
			}
			// Keep a reader attached without blocking until rush opens the
			// writer. The small payload fits without a concurrent drain.
			fd, err := syscall.Open(fifo, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
			if err != nil {
				t.Fatal(err)
			}
			reader := os.NewFile(uintptr(fd), fifo)
			defer reader.Close()
			args := []string{"-j", "1", "-o", fifo, "-c", "-C", journal}
			want := "hello\n"
			var env []string
			wantCode := 0
			if mode == "immediate" {
				args = append(args, "-I")
			} else if mode == "preview" {
				args = append(args, "--dry-run")
				want = "echo hello\n"
			} else if mode == "close-error" {
				env = []string{"RUSH_TEST_CLOSE_ERROR_FILE=" + fifo}
				wantCode = 1
			}
			stdout, stderr, code := runLegacyRush(t, "hello\n", env, append(args, "echo {}")...)
			if code != wantCode || stdout != "" || (wantCode == 0 && stderr != "") {
				t.Errorf("named-pipe output failed: exit=%d stdout=%q stderr=%q", code, stdout, stderr)
			}
			if mode == "close-error" && (!strings.Contains(stderr, "injected close failure") || strings.Contains(stderr, "restore output flags")) {
				t.Errorf("incorrect close diagnostics: %q", stderr)
			}
			if data, err := io.ReadAll(reader); err != nil || string(data) != want {
				t.Errorf("pipe output=%q err=%v; want %q", data, err, want)
			}
			if mode == "preview" {
				if _, err := os.Stat(journal); !os.IsNotExist(err) {
					t.Errorf("preview created records: %v", err)
				}
			} else {
				records, _ := readSuccCmds(journal)
				if _, ok := records["echo hello"]; ok != (mode != "close-error") {
					t.Errorf("incorrect successful-command records: %v", records)
				}
			}
		})
	}
}

func TestContinueRejectsFIFORecordBeforeOpening(t *testing.T) {
	dir := t.TempDir()
	journal, output := dir+"/record", dir+"/output"
	if err := syscall.Mkfifo(journal, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRushHelperProcess$", "--", "-c", "-C", journal, "-o", output, "echo {}")
	cmd.Env = append(os.Environ(), "RUSH_TEST_HELPER_PROCESS=1")
	cmd.Stdin = strings.NewReader("x\n")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		t.Fatal("rush blocked opening a FIFO as a successful-command file")
	}
	if err == nil || !strings.Contains(stderr.String(), "successful-command file must be a regular file") {
		t.Errorf("FIFO record accepted: error=%v stderr=%q", err, stderr.String())
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Errorf("output created before rejecting FIFO record: %v", err)
	}
	if info, err := os.Stat(journal); err != nil || info.Mode()&os.ModeNamedPipe == 0 {
		t.Errorf("FIFO record changed: info=%v, error=%v", info, err)
	}
}

func TestChildSignalExitStatus(t *testing.T) {
	for _, test := range []struct {
		name, command string
		args          []string
		want          int
	}{
		{"terminate", "kill -TERM $$", nil, 143},
		{"kill", "kill -KILL $$", nil, 137},
		{"interrupt", "kill -INT $$", nil, 130},
		{"hangup", "kill -HUP $$", nil, 129},
		{"retry", "kill -TERM $$", []string{"-r", "1"}, 143},
		{"propagation disabled", "kill -TERM $$", []string{"--propagate-exit-status=false"}, 0},
		{"stop on error", "kill -TERM $$", []string{"-e", "--propagate-exit-status=false"}, 143},
		{"normal high status", "exit 255", nil, 255},
		{"normal signal-like status", "exit 143", nil, 143},
	} {
		t.Run(test.name, func(t *testing.T) {
			args := append([]string{"-j", "1", "--cleanup-time", "0"}, test.args...)
			stdout, stderr, code := runLegacyRush(t, "x\n", nil, append(args, test.command)...)
			if code != test.want || stdout != "" {
				t.Fatalf("exit=%d; want %d; stdout=%q stderr=%q", code, test.want, stdout, stderr)
			}
		})
	}
}

func TestUnixProcessGroupCleanup(t *testing.T) {
	pidFile := t.TempDir() + "/child.pid"
	command := fmt.Sprintf("trap '' HUP INT TERM; sleep 30 & echo $! > %q; wait", pidFile)
	args := []string{"-test.run=^TestRushHelperProcess$", "--", "-j", "1", "--cleanup-time", "0", command}
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(), "RUSH_TEST_HELPER_PROCESS=1")
	cmd.Stdin = strings.NewReader("x\n")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := waitUnixPIDFile(t, pidFile, 3*time.Second)
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		t.Fatalf("rush did not exit; stderr=%s", stderr.String())
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err == syscall.ESRCH {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("descendant %d is still alive", pid)
}

func TestUnixProcessGroupCleanupAfterLeaderExits(t *testing.T) {
	pidFile := t.TempDir() + "/child.pid"
	command := fmt.Sprintf("trap 'exit 0' INT; sh -c 'trap \"\" INT; exec sleep 30' & echo $! > %q; wait", pidFile)
	args := []string{"-test.run=^TestRushHelperProcess$", "--", "-j", "1", "-t", "1", "--cleanup-time", "0", command}
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(), "RUSH_TEST_HELPER_PROCESS=1")
	cmd.Stdin = strings.NewReader("x\n")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := waitUnixPIDFile(t, pidFile, 3*time.Second)
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = syscall.Kill(pid, syscall.SIGKILL)
	})
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("rush hung after its command leader exited")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err == syscall.ESRCH {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("child survived after its command leader exited")
}

func TestWaitUnixPIDFileWaitsForCompleteRecord(t *testing.T) {
	for _, initial := range []string{"", "123"} {
		t.Run(fmt.Sprintf("initial=%q", initial), func(t *testing.T) {
			path := t.TempDir() + "/pid"
			if err := os.WriteFile(path, []byte(initial), 0600); err != nil {
				t.Fatal(err)
			}
			written := make(chan error, 1)
			go func() {
				// Model the gap between creating a PID file and completing it.
				time.Sleep(50 * time.Millisecond)
				written <- os.WriteFile(path, []byte("12345\n"), 0600)
			}()
			defer func() {
				if err := <-written; err != nil {
					t.Error(err)
				}
			}()
			if pid := waitUnixPIDFile(t, path, 2*time.Second); pid != 12345 {
				t.Fatalf("read incomplete PID: got %d; want 12345", pid)
			}
		})
	}
}

// waitUnixPIDFile waits for echo's newline, not just the shell's file creation.
func waitUnixPIDFile(t *testing.T, path string, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil && len(data) > 0 && data[len(data)-1] == '\n' {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(data)))
			if parseErr != nil {
				t.Fatal(parseErr)
			}
			return pid
		}
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("complete child PID was not recorded in %s within %s", path, timeout)
	return 0
}
