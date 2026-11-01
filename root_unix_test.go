//go:build linux

package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestInterruptStopsSkippedCommandGeneration(t *testing.T) {
	for _, resumed := range []bool{false, true} {
		t.Run(fmt.Sprintf("resumed=%t", resumed), func(t *testing.T) {
			args := []string{"--eta", "-j", "1", "--cleanup-time", "0"}
			input := " \n"
			if resumed {
				journal := t.TempDir() + "/success"
				if err := os.WriteFile(journal, []byte("echo old"+endMarkOfCMD), 0600); err != nil {
					t.Fatal(err)
				}
				args = append(args, "-c", "-C", journal)
				input = "old\n"
			} else {
				args = append(args, "-T", "b")
			}
			cmd := rushTestCommand(t, strings.Repeat(input, 100000), append(args, "echo {}")...)
			// Hold stderr in a pipe until the initial progress display arrives.
			// Backpressure prevents all skipped batches finishing before SIGINT.
			reader, writer, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			defer writer.Close()
			cmd.Stderr = writer
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if cmd.ProcessState == nil {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
				}
			})
			_ = writer.Close()
			if err := reader.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
				t.Fatal(err)
			}
			initial := make([]byte, 1024)
			n, err := reader.Read(initial)
			if err != nil || !bytes.Contains(initial[:n], []byte("processed jobs:")) {
				t.Fatalf("missing initial progress: %q, %v", initial[:n], err)
			}
			if err := cmd.Process.Signal(os.Interrupt); err != nil {
				t.Fatal(err)
			}
			if err := reader.SetReadDeadline(time.Time{}); err != nil {
				t.Fatal(err)
			}
			output := make(chan []byte, 1)
			readError := make(chan error, 1)
			go func() {
				data, err := io.ReadAll(reader)
				output <- data
				readError <- err
			}()
			waitForStartedRush(t, cmd, 10*time.Second)
			stderr := string(<-output)
			if err := <-readError; err != nil {
				t.Fatal(err)
			}
			if cmd.ProcessState.ExitCode() != 130 {
				t.Fatalf("exit=%d; want 130", cmd.ProcessState.ExitCode())
			}
			if strings.Contains(stderr, "100%") || strings.Contains(stderr, "(0/0)") {
				t.Fatal("processed all skipped batches after interruption")
			}
			if stdout := cmd.Stdout.(*lockedBuffer).String(); stdout != "" {
				t.Fatalf("unexpected job output: %q", stdout)
			}
		})
	}
}

func TestETACountsSkippedPartialBatchBeforeOtherJobsFinish(t *testing.T) {
	journal := t.TempDir() + "/success"
	if err := os.WriteFile(journal, []byte("sleep 30; echo old"+endMarkOfCMD), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := rushTestCommand(t, "new\nother\nold\n", "--eta", "-j", "1", "-n", "2", "-c", "-C", journal,
		"--cleanup-time", "0", "sleep 30; echo {1}")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Signal(os.Interrupt)
		waitForStartedRush(t, cmd, 5*time.Second)
		if cmd.ProcessState.ExitCode() != 130 || strings.Contains(cmd.Stderr.(*lockedBuffer).String(), "100%") {
			t.Errorf("interruption incorrectly finished progress: exit=%d stderr=%s", cmd.ProcessState.ExitCode(), cmd.Stderr)
		}
	}()
	waitForBufferContains(t, cmd.Stderr.(*lockedBuffer), "(1/2,", time.Second)
}

func TestCommandExpansionFailureStopsActiveProcesses(t *testing.T) {
	for _, batch := range []int{1, 2} {
		t.Run(strconv.Itoa(batch), func(t *testing.T) {
			dir := t.TempDir()
			pidFile, journal := dir+"/pids", dir+"/success"
			original := "echo old" + endMarkOfCMD
			if err := os.WriteFile(journal, []byte(original), 0600); err != nil {
				t.Fatal(err)
			}
			// Waiting for the PID file makes the expansion failure occur only
			// after the long-running command has started its descendant.
			command := fmt.Sprintf(`case "{1}" in hold*) trap '' INT; sleep 30 & echo "$$ $!" > %q; wait;; *) while [ ! -s %q ]; do sleep 0.01; done;; esac; echo '{2@[}'`, pidFile, pidFile)
			var input strings.Builder
			for _, record := range []string{"hold", "fast", "fast", "fast"} {
				fmt.Fprintln(&input, record)
				if batch == 2 {
					fmt.Fprintln(&input, "padding")
				}
			}
			fmt.Fprintln(&input, "bad=extra")
			t.Cleanup(func() {
				data, _ := os.ReadFile(pidFile)
				fields := strings.Fields(string(data))
				if len(fields) == 2 {
					parent, _ := strconv.Atoi(fields[0])
					child, _ := strconv.Atoi(fields[1])
					if parent > 0 && (processRunning(parent) || processRunning(child)) {
						_ = syscall.Kill(-parent, syscall.SIGKILL)
					}
				}
			})
			cmd := rushTestCommand(t, input.String(), "-j", "2", "-n", strconv.Itoa(batch), "-d", "=", "-c", "-C", journal,
				"--cleanup-time", "0", "--propagate-exit-status=false", command)
			waitForRush(t, cmd, 5*time.Second)
			data, err := os.ReadFile(pidFile)
			if err != nil {
				t.Fatal(err)
			}
			pids := strings.Fields(string(data))
			if len(pids) != 2 {
				t.Fatalf("unexpected PID record: %q", data)
			}
			for _, text := range pids {
				pid, err := strconv.Atoi(text)
				if err != nil {
					t.Fatal(err)
				}
				assertProcessStopped(t, pid, time.Second)
			}
			if cmd.ProcessState.ExitCode() != 1 || !strings.Contains(cmd.Stderr.(*lockedBuffer).String(), "fill command") {
				t.Fatalf("exit=%d stderr=%s", cmd.ProcessState.ExitCode(), cmd.Stderr)
			}
			data, err = os.ReadFile(journal)
			if err != nil || !bytes.HasPrefix(data, []byte(original)) || len(data) <= len(original) {
				t.Fatalf("completed command records were lost: %q, err=%v", data, err)
			}
		})
	}
}

// lowFileLimitCommand applies both limits before exec so the Go runtime cannot
// raise the soft limit again. The parent test process keeps its original limit.
func lowFileLimitCommand(t *testing.T, stdin string, args ...string) *exec.Cmd {
	t.Helper()
	cmd := rushTestCommand(t, stdin, args...)
	cmd.Args = append([]string{"sh", "-c", `ulimit -n 64 && exec "$@"`, "sh"}, cmd.Args...)
	var err error
	cmd.Path, err = exec.LookPath("sh")
	if err != nil {
		t.Fatal(err)
	}
	return cmd
}

func TestLowFileLimitConcurrentCommands(t *testing.T) {
	const count = 48
	var input strings.Builder
	for i := 1; i <= count; i++ {
		fmt.Fprintln(&input, i)
	}
	for _, mode := range []string{"buffered", "immediate", "pipe", "spill", "inherited"} {
		t.Run(mode, func(t *testing.T) {
			args := []string{"-j", "48", "-k"}
			command := `sleep 0.05; printf '%s\n' {} | cat | cat`
			want := input.String()
			switch mode {
			case "immediate":
				args = append(args, "-I")
			case "pipe":
				args = append(args, "--pipe")
				command = `sleep 0.05; cat | cat | cat`
			case "spill":
				// Slow the first job to retain later jobs' spill files under -k.
				command = `if [ {} = 1 ]; then sleep 0.2; fi; head -c 1100000 /dev/zero; echo {}`
				var expected strings.Builder
				for i := 1; i <= count; i++ {
					expected.WriteString(strings.Repeat("\x00", 1100000))
					fmt.Fprintln(&expected, i)
				}
				want = expected.String()
			}
			cmd := lowFileLimitCommand(t, input.String(), append(args, command)...)
			if mode == "inherited" {
				f, err := os.Open(os.DevNull)
				if err != nil {
					t.Fatal(err)
				}
				defer f.Close()
				// Each entry becomes a distinct inherited descriptor in rush.
				for i := 0; i < 24; i++ {
					cmd.ExtraFiles = append(cmd.ExtraFiles, f)
				}
			}
			waitForRush(t, cmd, 15*time.Second)
			stderr := cmd.Stderr.(*lockedBuffer).String()
			if cmd.ProcessState.ExitCode() != 0 || strings.Contains(stderr, "too many open files") {
				t.Fatalf("exit=%d stderr=%s", cmd.ProcessState.ExitCode(), stderr)
			}
			got := cmd.Stdout.(*lockedBuffer).String()
			if mode == "immediate" {
				assertLineSet(t, got, strings.Fields(input.String()))
			} else if got != want {
				t.Fatalf("output differs: got %d bytes, want %d", len(got), len(want))
			}
		})
	}
}

func TestLowFileLimitRetryOutput(t *testing.T) {
	// Failed attempts retain spill files until the logical job finishes. More
	// than one such job would exceed this limit without a retry-aware budget.
	cmd := lowFileLimitCommand(t, "1\n2\n3\n", "-j", "3", "-k", "-r", "24",
		`sleep 0.01; head -c 1100000 /dev/zero; echo {}; exit 7`)
	waitForRush(t, cmd, 15*time.Second)
	stderr := cmd.Stderr.(*lockedBuffer).String()
	if cmd.ProcessState.ExitCode() != 7 || strings.Contains(stderr, "too many open files") {
		t.Fatalf("exit=%d stderr=%s", cmd.ProcessState.ExitCode(), stderr)
	}
	got := cmd.Stdout.(*lockedBuffer).String()
	for i := 1; i <= 3; i++ {
		block := strings.Repeat("\x00", 1100000) + strconv.Itoa(i) + "\n"
		for attempt := 0; attempt < 25; attempt++ {
			if !strings.HasPrefix(got, block) {
				t.Fatalf("missing or changed output for job %d attempt %d", i, attempt+1)
			}
			got = got[len(block):]
		}
	}
	if len(got) != 0 {
		t.Fatalf("unexpected extra output: %d bytes", len(got))
	}
}

func TestLowFileLimitInsufficientBudget(t *testing.T) {
	cmd := lowFileLimitCommand(t, "x\n", "-j", "1", "-r", "100", "echo must-not-start")
	waitForRush(t, cmd, 5*time.Second)
	if cmd.ProcessState.ExitCode() != 1 || cmd.Stdout.(*lockedBuffer).String() != "" ||
		!strings.Contains(cmd.Stderr.(*lockedBuffer).String(), "open-file limit (64) is too low") {
		t.Fatalf("exit=%d stderr=%s", cmd.ProcessState.ExitCode(), cmd.Stderr.(*lockedBuffer).String())
	}
	for _, flag := range []string{"-I", "--dry-run", "-e"} {
		cmd = lowFileLimitCommand(t, "x\n", "-j", "1", "-r", "100", flag, "echo {}")
		waitForRush(t, cmd, 5*time.Second)
		if cmd.ProcessState.ExitCode() != 0 || !strings.Contains(cmd.Stdout.(*lockedBuffer).String(), "x\n") {
			t.Fatalf("%s: exit=%d stderr=%s", flag, cmd.ProcessState.ExitCode(), cmd.Stderr.(*lockedBuffer).String())
		}
	}
}

func TestLowFileLimitHugeJobCount(t *testing.T) {
	maxInt := strconv.Itoa(int(^uint(0) >> 1))
	cmd := lowFileLimitCommand(t, "x\n", "-j", maxInt, "echo {}")
	waitForRush(t, cmd, 5*time.Second)
	if cmd.ProcessState.ExitCode() != 0 || cmd.Stdout.(*lockedBuffer).String() != "x\n" {
		t.Fatalf("exit=%d stderr=%s", cmd.ProcessState.ExitCode(), cmd.Stderr.(*lockedBuffer).String())
	}
}

func TestOutputPreservedWhenInputReadOrSetupFails(t *testing.T) {
	for _, failure := range []string{"input-read", "file-limit"} {
		t.Run(failure, func(t *testing.T) {
			path := t.TempDir() + "/output"
			original := []byte("keep previous output\n")
			if err := os.WriteFile(path, original, 0600); err != nil {
				t.Fatal(err)
			}
			var cmd *exec.Cmd
			if failure == "input-read" {
				// This file can be statted and opened, but reading address zero fails.
				cmd = rushTestCommand(t, "", "-i", "/proc/self/mem", "-o", path, "echo {}")
			} else {
				cmd = lowFileLimitCommand(t, "x\n", "-r", "100", "-o", path, "echo {}")
			}
			waitForRush(t, cmd, 5*time.Second)
			if cmd.ProcessState.ExitCode() == 0 {
				t.Fatal("expected input or setup failure")
			}
			data, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(data, original) {
				t.Fatalf("output changed before execution: %q, err=%v", data, err)
			}
		})
	}
}

func TestLowFileLimitManyInputFiles(t *testing.T) {
	path := t.TempDir() + "/input"
	if err := os.WriteFile(path, []byte("x\n"), 0600); err != nil {
		t.Fatal(err)
	}
	args := []string{"-j", "1", "-k"}
	for i := 0; i < 100; i++ {
		args = append(args, "-i", path)
	}
	cmd := lowFileLimitCommand(t, "", append(args, "echo {}")...)
	waitForRush(t, cmd, 10*time.Second)
	if cmd.ProcessState.ExitCode() != 0 || cmd.Stdout.(*lockedBuffer).String() != strings.Repeat("x\n", 100) {
		t.Fatalf("exit=%d stderr=%s", cmd.ProcessState.ExitCode(), cmd.Stderr.(*lockedBuffer).String())
	}
}

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

func TestInterruptDuringStartDelayExits(t *testing.T) {
	startedFile := t.TempDir() + "/started"
	cmd := rushTestCommand(t, "1\n2\n", "-j", "2", "--delay", "30", "--cleanup-time", "0", fmt.Sprintf("echo {} >> %q", startedFile))
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, startedFile, 2*time.Second)
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		_ = cmd.Process.Kill()
		t.Fatal(err)
	}
	waitForStartedRush(t, cmd, 5*time.Second)
	if code := cmd.ProcessState.ExitCode(); code != 130 {
		t.Fatalf("exit code: %d; want 130\nstderr:\n%s", code, cmd.Stderr)
	}
	data, err := os.ReadFile(startedFile)
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Count(string(data), "\n"); lines != 1 {
		t.Fatalf("started commands=%d; want 1", lines)
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
	for _, input := range []string{"sleep 30\n", "exit 7\nsleep 30\n"} {
		cmd := rushTestCommand(t, input, "-j", "1", "-t", "1", "--cleanup-time", "0", "--propagate-exit-status=false", "{}")
		waitForRush(t, cmd, 5*time.Second)
		if code := cmd.ProcessState.ExitCode(); code != 124 {
			t.Errorf("input=%q: exit code: %d; want 124\nstderr:\n%s", input, code, cmd.Stderr)
		}
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
