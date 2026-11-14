//go:build linux

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestProcessWaitBeforeAllJobs(t *testing.T) {
	for _, test := range []struct {
		appear, regexp bool
	}{{true, false}, {false, false}, {true, true}, {false, true}} {
		t.Run(fmt.Sprintf("appear=%t/regexp=%t", test.appear, test.regexp), func(t *testing.T) {
			dir := t.TempDir()
			name := fmt.Sprintf("rush-wait-%x-worker", time.Now().UnixNano())
			worker := filepath.Join(dir, name)
			data, err := os.ReadFile("/bin/sleep")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(worker, data, 0700); err != nil {
				t.Fatal(err)
			}
			startWorker := func() *exec.Cmd {
				p := exec.Command(worker, "30")
				if err := p.Start(); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = p.Process.Kill(); _ = p.Wait() })
				return p
			}
			flag := "--wait-for-process"
			pattern := name
			var matchFlags []string
			var first, second *exec.Cmd
			if !test.appear {
				flag = "--wait-for-process-exit"
				first, second = startWorker(), startWorker()
			}
			if test.regexp {
				flag = "-W"
				if !test.appear {
					flag = "-w"
				}
				pattern = regexp.QuoteMeta(name[15:]) + "$"
				matchFlags = []string{"-R"}
			}
			output, journal := filepath.Join(dir, "output"), filepath.Join(dir, "journal")
			if err := os.WriteFile(output, []byte("preserved"), 0600); err != nil {
				t.Fatal(err)
			}
			// Jobs reuse the watched name: after the initial release they must
			// run normally, without reapplying the disappearance condition.
			command := shellQuote(worker) + " 0.1; echo {}"
			args := append(matchFlags, "--verbose", flag, pattern, "-u", "0.1", "-U", "0.1", "-k", "-j", "2", "-c", "-C", journal, "-o", output, command)
			cmd := rushTestCommand(t, "1\n2\n3\n", args...)
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = cmd.Process.Kill() })
			waitForProcessWaitNotice(t, cmd)
			assertUntouched := func() {
				t.Helper()
				if data, err := os.ReadFile(output); err != nil || string(data) != "preserved" {
					t.Fatalf("output changed while waiting: %q, %v", data, err)
				}
				if _, err := os.Stat(journal); !os.IsNotExist(err) {
					t.Fatalf("journal created while waiting: %v", err)
				}
			}
			time.Sleep(150 * time.Millisecond)
			assertUntouched()
			if test.appear {
				startWorker()
			} else {
				_ = first.Process.Kill()
				_ = first.Wait()
				time.Sleep(1100 * time.Millisecond)
				assertUntouched()
				_ = second.Process.Kill()
				_ = second.Wait()
			}
			waitForStartedRush(t, cmd, 5*time.Second)
			if cmd.ProcessState.ExitCode() != 0 {
				t.Fatalf("rush failed: %v, %s", cmd.ProcessState, cmd.Stderr)
			}
			if data, err := os.ReadFile(output); err != nil || normalizedLines(string(data)) != "1\n2\n3" {
				t.Fatalf("jobs did not run after release: %q, %v", data, err)
			}
		})
	}
}

func TestProcessWaitInterrupt(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		output := filepath.Join(t.TempDir(), "output")
		cmd := rushTestCommand(t, "x\n", "--verbose", "--wait-for-process", "rush-nonexistent-test-worker", "-o", output, "echo executed")
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill() })
		waitForProcessWaitNotice(t, cmd)
		if err := cmd.Process.Signal(sig); err != nil {
			t.Fatal(err)
		}
		waitForStartedRush(t, cmd, 3*time.Second)
		if got := cmd.ProcessState.ExitCode(); got != terminationStatus(sig) {
			t.Fatalf("signal %s: exit=%d, stderr=%s", sig, got, cmd.Stderr)
		}
		if _, err := os.Stat(output); !os.IsNotExist(err) {
			t.Fatalf("interrupt created output: %v", err)
		}
	}
}

// waitForProcessWaitNotice observes readiness after the signal handler is installed.
func waitForProcessWaitNotice(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !strings.Contains(fmt.Sprint(cmd.Stderr), "waiting for process") {
		if time.Now().After(deadline) {
			t.Fatalf("rush did not start waiting: %s", cmd.Stderr)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
