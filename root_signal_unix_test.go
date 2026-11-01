//go:build linux

package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestInterruptibleOutputRestoresFlags(t *testing.T) {
	for _, nonblock := range []bool{false, true} {
		t.Run(fmt.Sprintf("nonblock=%t", nonblock), func(t *testing.T) {
			fds := make([]int, 2)
			if err := syscall.Pipe(fds); err != nil {
				t.Fatal(err)
			}
			syscall.CloseOnExec(fds[0])
			syscall.CloseOnExec(fds[1])
			if err := syscall.SetNonblock(fds[1], nonblock); err != nil {
				t.Fatal(err)
			}
			reader := os.NewFile(uintptr(fds[0]), "reader")
			writer := os.NewFile(uintptr(fds[1]), "output")
			defer reader.Close()
			defer writer.Close()
			before, err := unix.FcntlInt(uintptr(fds[1]), unix.F_GETFL, 0)
			if err != nil {
				t.Fatal(err)
			}
			output, cleanup, err := newInterruptibleOutput(writer)
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			if _, err := output.WriteString("before"); err != nil {
				t.Fatal(err)
			}
			if err := output.SetWriteDeadline(time.Now()); err != nil {
				t.Fatal(err)
			}
			if _, err := output.WriteString("blocked"); !errors.Is(err, os.ErrDeadlineExceeded) {
				t.Fatalf("expired write deadline: %v", err)
			}
			if err := cleanup(); err != nil {
				t.Fatal(err)
			}
			if output != writer {
				if _, err := output.Stat(); !errors.Is(err, os.ErrClosed) {
					t.Errorf("output descriptor copy was not closed: %v", err)
				}
			}
			after, err := unix.FcntlInt(uintptr(fds[1]), unix.F_GETFL, 0)
			if err != nil || before != after {
				t.Errorf("output flags changed: before=%x after=%x err=%v", before, after, err)
			}
			if _, err := writer.WriteString("after"); err != nil {
				t.Fatalf("original output handle was not reusable: %v", err)
			}
		})
	}
}

func TestClosedOutputPipeStopsCommands(t *testing.T) {
	for _, mode := range []struct {
		sink      string
		immediate bool
	}{{"stdout", true}, {"stderr", true}, {"stderr", false}} {
		sink := mode.sink
		for _, nonblock := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/immediate=%t/nonblocking=%t", sink, mode.immediate, nonblock), func(t *testing.T) {
				fds := make([]int, 2)
				if err := syscall.Pipe(fds); err != nil {
					t.Fatal(err)
				}
				syscall.CloseOnExec(fds[0])
				syscall.CloseOnExec(fds[1])
				reader := os.NewFile(uintptr(fds[0]), "reader")
				writer := os.NewFile(uintptr(fds[1]), "writer")
				defer reader.Close()
				defer writer.Close()
				// Construct the Go file before changing flags: os/exec calls Fd,
				// which resets files created by os.Pipe to blocking mode.
				fd := writer.Fd()
				if err := syscall.SetNonblock(int(fd), nonblock); err != nil {
					t.Fatal(err)
				}
				dir := t.TempDir()
				pidFile, release, journal := dir+"/pid", dir+"/release", dir+"/records"
				original := encodeSuccessfulCommand("echo previous") + endMarkOfCMD
				if err := os.WriteFile(journal, []byte(original), 0600); err != nil {
					t.Fatal(err)
				}
				redirect := ""
				if sink == "stderr" {
					redirect = " >&2"
				}
				// Keep the command running after its write so an abrupt rush exit
				// would leave a live process, rather than hiding behind a quick exit.
				command := fmt.Sprintf("echo $$ > %q; while [ ! -f %q ]; do sleep 0.01; done; echo output%s; exec sleep 30", pidFile, release, redirect)
				args := []string{"--cleanup-time", "0", "-c", "-C", journal}
				if mode.immediate {
					args = append(args, "-I")
				}
				cmd := rushTestCommand(t, "x\n", append(args, command)...)
				if sink == "stdout" {
					cmd.Stdout = writer
				} else {
					cmd.Stderr = writer
				}
				if err := cmd.Start(); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if cmd.ProcessState == nil {
						_ = cmd.Process.Kill()
						_ = cmd.Wait()
					}
				})
				pid := waitForPIDFile(t, pidFile, 2*time.Second)
				t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
				_ = reader.Close()
				if err := os.WriteFile(release, nil, 0600); err != nil {
					t.Fatal(err)
				}
				waitForStartedRush(t, cmd, 3*time.Second)
				if status := cmd.ProcessState.ExitCode(); status != 1 {
					t.Errorf("rush did not report an I/O failure: exit=%d, state=%s", status, cmd.ProcessState)
				}
				assertProcessStopped(t, pid, time.Second)
				flags, err := unix.FcntlInt(fd, unix.F_GETFL, 0)
				if err != nil || (flags&syscall.O_NONBLOCK != 0) != nonblock {
					t.Errorf("inherited flags changed: flags=%x err=%v", flags, err)
				}
				if data, err := os.ReadFile(journal); err != nil || string(data) != original {
					t.Errorf("failed output changed records: %q err=%v", data, err)
				}
			})
		}
	}
}

func TestSecondInterruptUnblocksOutput(t *testing.T) {
	for _, test := range []struct {
		name                      string
		immediate, merged, stderr bool
	}{{"buffered", false, false, false}, {"immediate", true, false, false}, {"merged ETA", true, true, false}, {"stderr buffered", false, false, true}, {"stderr immediate", true, false, true}} {
		t.Run(test.name, func(t *testing.T) {
			reader, writer, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			defer writer.Close()
			writerFD := writer.Fd()
			capacity, err := unix.FcntlInt(writerFD, unix.F_GETPIPE_SZ, 0)
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			journal, pidFile := dir+"/success", dir+"/child.pid"
			original := []byte("already complete" + endMarkOfCMD)
			if err := os.WriteFile(journal, original, 0600); err != nil {
				t.Fatal(err)
			}
			args := []string{"-j", "1", "--cleanup-time", "0", "-c", "-C", journal}
			if test.immediate {
				args = append(args, "-I")
			}
			if test.merged {
				args = append(args, "--eta")
			}
			command := fmt.Sprintf("echo $$ > %q; exec head -c %d /dev/zero", pidFile, max(capacity*4, 4<<20))
			if test.stderr {
				command += " >&2"
			}
			args = append(args, command)
			cmd := rushTestCommand(t, "x\n", args...)
			// Keep the reader open without consuming any output.
			if test.stderr {
				cmd.Stderr = writer
			} else {
				cmd.Stdout = writer
			}
			if test.merged {
				cmd.Stderr = writer
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if cmd.ProcessState == nil {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
				}
			})
			pid := waitForPIDFile(t, pidFile, 2*time.Second)
			t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
			waitForFullOutputPipe(t, writerFD)
			if err := cmd.Process.Signal(os.Interrupt); err != nil {
				t.Fatal(err)
			}
			if test.merged || test.stderr {
				// The output-producing child cannot finish while its pipe is
				// blocked. Its death confirms the first interrupt was handled.
				assertProcessStopped(t, pid, 2*time.Second)
			} else {
				waitForBufferContains(t, cmd.Stderr.(*lockedBuffer), "received an interrupt", 2*time.Second)
			}
			if err := cmd.Process.Signal(os.Interrupt); err != nil {
				t.Fatal(err)
			}
			waitForStartedRush(t, cmd, 3*time.Second)
			if cmd.ProcessState.ExitCode() != 130 {
				t.Errorf("exit=%d; want 130; stderr=%s", cmd.ProcessState.ExitCode(), cmd.Stderr)
			}
			assertProcessStopped(t, pid, time.Second)
			flags, err := unix.FcntlInt(writerFD, unix.F_GETFL, 0)
			if err != nil || flags&syscall.O_NONBLOCK != 0 {
				t.Errorf("inherited output flags were not restored: flags=%x err=%v", flags, err)
			}
			records, err := os.ReadFile(journal)
			if err != nil || !bytes.Equal(records, original) {
				t.Errorf("incomplete output retained success records: %q, err=%v", records, err)
			}
		})
	}
}

// waitForFullOutputPipe checks capacity without draining the blocked output.
func waitForFullOutputPipe(t *testing.T, fd uintptr) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLOUT}}
		ready, err := unix.Poll(poll, 0)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if ready == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("output pipe did not fill")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestInterruptUnblocksLoggerBeforeJobStart(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	fd := writer.Fd()
	if _, err := unix.FcntlInt(fd, unix.F_SETPIPE_SZ, 4096); err != nil {
		t.Fatal(err)
	}
	marker := t.TempDir() + "/started"
	// The verbose start notice fills stderr before the child can be launched.
	command := fmt.Sprintf("echo started > %q # %s", marker, strings.Repeat("x", 16<<10))
	cmd := rushTestCommand(t, "x\n", "-j", "1", "--verbose", command)
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
	waitForFullOutputPipe(t, fd)
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	// Different signals cannot coalesce while the signal goroutine is scheduled.
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	waitForStartedRush(t, cmd, 3*time.Second)
	if code := cmd.ProcessState.ExitCode(); code != 130 && code != 143 {
		t.Fatalf("exit=%d; want a handled termination signal", code)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("cancelled command started: %v", err)
	}
	flags, err := unix.FcntlInt(fd, unix.F_GETFL, 0)
	if err != nil || flags&syscall.O_NONBLOCK != 0 {
		t.Fatalf("stderr flags were not restored: flags=%x err=%v", flags, err)
	}
}

func TestFIFOInputWaitsForWriter(t *testing.T) {
	for _, data := range []string{"ready\n", ""} {
		t.Run(fmt.Sprintf("empty=%t", data == ""), func(t *testing.T) {
			fifo := t.TempDir() + "/input"
			if err := syscall.Mkfifo(fifo, 0600); err != nil {
				t.Fatal(err)
			}
			cmd := rushTestCommand(t, "", "-i", fifo, "echo {}")
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if cmd.ProcessState == nil {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
				}
			})
			waitForOpenPath(t, cmd.Process.Pid, fifo, 2*time.Second)
			// Leave the reader alone long enough to encounter the initial EOF.
			time.Sleep(30 * time.Millisecond)
			fd, err := syscall.Open(fifo, syscall.O_WRONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
			if err != nil {
				t.Fatalf("rush did not wait for the FIFO writer: %v", err)
			}
			writer := os.NewFile(uintptr(fd), "writer")
			if _, err := writer.WriteString(data); err != nil {
				_ = writer.Close()
				t.Fatal(err)
			}
			_ = writer.Close()
			waitForStartedRush(t, cmd, 5*time.Second)
			if cmd.ProcessState.ExitCode() != 0 || cmd.Stdout.(*lockedBuffer).String() != data {
				t.Fatalf("FIFO input lost: exit=%d stdout=%s stderr=%s", cmd.ProcessState.ExitCode(), cmd.Stdout, cmd.Stderr)
			}
		})
	}
}

func TestInterruptFIFOWithoutWriter(t *testing.T) {
	fifo := t.TempDir() + "/input"
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	cmd := rushTestCommand(t, "", "-i", fifo, "echo {}")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	waitForOpenPath(t, cmd.Process.Pid, fifo, 2*time.Second)
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	waitForStartedRush(t, cmd, 5*time.Second)
	if cmd.ProcessState.ExitCode() != 130 {
		t.Fatalf("exit=%d; want 130; stderr=%s", cmd.ProcessState.ExitCode(), cmd.Stderr)
	}
}

func TestInputFileIsNotInheritedByChildren(t *testing.T) {
	path := t.TempDir() + "/input"
	if err := os.WriteFile(path, []byte("input"), 0600); err != nil {
		t.Fatal(err)
	}
	file, err := openInputFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	cmd := exec.Command("sh", "-c", fmt.Sprintf("test ! -e /proc/self/fd/%d", file.Fd()))
	if err := cmd.Run(); err != nil {
		t.Fatalf("child inherited the open input descriptor: %v", err)
	}
}

func TestInputRestoresInheritedFlags(t *testing.T) {
	for _, nonblock := range []bool{false, true} {
		for _, interrupt := range []bool{false, true} {
			t.Run(fmt.Sprintf("nonblock=%t/interrupt=%t", nonblock, interrupt), func(t *testing.T) {
				// Use raw pipe descriptors so os.File.Fd does not itself switch
				// a Go-created pipe from nonblocking to blocking mode.
				fds := make([]int, 2)
				if err := syscall.Pipe(fds); err != nil {
					t.Fatal(err)
				}
				syscall.CloseOnExec(fds[0])
				syscall.CloseOnExec(fds[1])
				if err := syscall.SetNonblock(fds[0], nonblock); err != nil {
					t.Fatal(err)
				}
				reader := os.NewFile(uintptr(fds[0]), "input")
				writer := os.NewFile(uintptr(fds[1]), "writer")
				defer reader.Close()
				defer writer.Close()
				before, err := unix.FcntlInt(uintptr(fds[0]), unix.F_GETFL, 0)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := writer.WriteString("ready\n"); err != nil {
					t.Fatal(err)
				}
				if !interrupt {
					_ = writer.Close()
				}
				cmd := rushTestCommand(t, "", "--cleanup-time", "0", "echo {}")
				cmd.Stdin = reader
				if err := cmd.Start(); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if cmd.ProcessState == nil {
						_ = cmd.Process.Kill()
						_ = cmd.Wait()
					}
				})
				if interrupt {
					// Wait until rush has consumed the marker and is waiting for
					// more input; keep the writer open so EOF cannot finish it.
					deadline := time.Now().Add(3 * time.Second)
					for {
						remaining, err := unix.IoctlGetInt(fds[0], unix.TIOCINQ)
						if err != nil {
							t.Fatal(err)
						}
						if remaining == 0 {
							break
						}
						if time.Now().After(deadline) {
							t.Fatal("rush did not read the input marker")
						}
						time.Sleep(10 * time.Millisecond)
					}
					if err := cmd.Process.Signal(os.Interrupt); err != nil {
						t.Fatal(err)
					}
				}
				waitForStartedRush(t, cmd, 5*time.Second)
				wantCode := 0
				if interrupt {
					wantCode = 130
				}
				if cmd.ProcessState.ExitCode() != wantCode {
					t.Fatalf("exit=%d; want %d; stderr=%s", cmd.ProcessState.ExitCode(), wantCode, cmd.Stderr)
				}
				after, err := unix.FcntlInt(uintptr(fds[0]), unix.F_GETFL, 0)
				if err != nil || after != before {
					t.Fatalf("inherited input flags changed: before=%#x after=%#x err=%v", before, after, err)
				}
			})
		}
	}
}

func TestInputRestoresFlagsAfterReadFailure(t *testing.T) {
	file, err := os.OpenFile(t.TempDir()+"/write-only", os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	fd := file.Fd()
	before, err := unix.FcntlInt(fd, unix.F_GETFL, 0)
	if err != nil {
		t.Fatal(err)
	}
	cmd := rushTestCommand(t, "", "echo {}")
	cmd.Stdin = file
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	writerFD := writer.Fd() // Give the child a blocking inherited stdout.
	cmd.Stdout = writer
	waitForRush(t, cmd, 5*time.Second)
	if cmd.ProcessState.ExitCode() == 0 || !strings.Contains(cmd.Stderr.(*lockedBuffer).String(), "read input data") {
		t.Fatalf("read failure was not reported: exit=%d stderr=%s", cmd.ProcessState.ExitCode(), cmd.Stderr)
	}
	after, err := unix.FcntlInt(fd, unix.F_GETFL, 0)
	if err != nil || after != before {
		t.Fatalf("inherited input flags changed: before=%#x after=%#x err=%v", before, after, err)
	}
	flags, err := unix.FcntlInt(writerFD, unix.F_GETFL, 0)
	if err != nil || flags&syscall.O_NONBLOCK != 0 {
		t.Fatalf("inherited output flags changed after input failure: flags=%x err=%v", flags, err)
	}
}

func TestInputRestoreDoesNotLeakDescriptors(t *testing.T) {
	args := []string{"-j", "1"}
	for i := 0; i < 100; i++ {
		args = append(args, "-i", "-")
	}
	cmd := lowFileLimitCommand(t, "ready\n", append(args, "echo {}")...)
	waitForRush(t, cmd, 5*time.Second)
	if cmd.ProcessState.ExitCode() != 0 || cmd.Stdout.(*lockedBuffer).String() != "ready\n" {
		t.Fatalf("input restoration exhausted descriptors: exit=%d stderr=%s", cmd.ProcessState.ExitCode(), cmd.Stderr)
	}
}

func TestFatalFailureCancelsBeforeBlockedLogging(t *testing.T) {
	for _, test := range []struct {
		name                             string
		verbose, writeFailure, immediate bool
	}{
		{"exit error", false, false, false},
		{"verbose exit error", true, false, false},
		{"buffered output error", false, true, false},
		{"verbose immediate output error", true, true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			reader, writer, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			defer writer.Close()
			fd := writer.Fd()
			if _, err := unix.FcntlInt(fd, unix.F_SETPIPE_SZ, 4096); err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			longPID, failedPID, trigger := dir+"/long.pid", dir+"/failed.pid", dir+"/fail"
			failure, status := "exit 7", 7
			args := []string{"-j", "2", "--cleanup-time", "0"}
			if test.writeFailure {
				failure, status = "printf output", 1
				args = append(args, "-o", "/dev/full")
			} else {
				args = append(args, "-e")
			}
			if test.immediate {
				args = append(args, "-I")
			}
			input := fmt.Sprintf("echo $$ > %q; exec sleep 60\necho $$ > %q; while [ ! -e %q ]; do sleep 0.01; done; %s\n", longPID, failedPID, trigger, failure)
			if test.verbose {
				args = append(args, "--verbose")
			}
			cmd := rushTestCommand(t, input, append(args, "{}")...)
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
			pid := waitForPIDFile(t, longPID, 2*time.Second)
			t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
			otherPID := waitForPIDFile(t, failedPID, 2*time.Second)
			t.Cleanup(func() { _ = syscall.Kill(otherPID, syscall.SIGKILL) })
			flags, err := unix.FcntlInt(fd, unix.F_GETFL, 0)
			if err != nil || flags&syscall.O_NONBLOCK == 0 {
				t.Fatalf("stderr was not prepared: flags=%x err=%v", flags, err)
			}
			// Fill even the last few bytes so a short completion/error log blocks.
			for size := 4096; size > 0; size /= 2 {
				data := bytes.Repeat([]byte("x"), size)
				for {
					_, err := unix.Write(int(fd), data)
					if errors.Is(err, syscall.EAGAIN) {
						break
					}
					if errors.Is(err, syscall.EINTR) {
						continue
					}
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := os.WriteFile(trigger, nil, 0600); err != nil {
				t.Fatal(err)
			}
			assertProcessStopped(t, otherPID, 2*time.Second)
			// Do not drain stderr or send an interrupt until automatic cleanup is proven.
			assertProcessStopped(t, pid, 2*time.Second)
			drained := make(chan struct{})
			go func() { _, _ = io.Copy(io.Discard, reader); close(drained) }()
			waitForStartedRush(t, cmd, 3*time.Second)
			_ = writer.Close()
			<-drained
			if cmd.ProcessState.ExitCode() != status {
				t.Fatalf("exit=%d; want first failure %d", cmd.ProcessState.ExitCode(), status)
			}
		})
	}
}
