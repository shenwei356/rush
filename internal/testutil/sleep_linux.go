// Package testutil provides subprocess fixtures for Rush tests.
package testutil

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// SleepMain lets a renamed test binary act as a quiet, single-process sleeper.
func SleepMain(m *testing.M) {
	if len(os.Args) == 2 || (len(os.Args) == 3 && os.Args[2] == "--ready") {
		if duration, ok := strings.CutPrefix(os.Args[1], "--test-sleep="); ok {
			delay, err := time.ParseDuration(duration)
			if err != nil {
				os.Exit(2)
			}
			if len(os.Args) == 3 {
				_, _ = os.Stdout.Write([]byte{'R'})
			}
			time.Sleep(delay)
			os.Exit(0)
		}
	}
	os.Exit(m.Run())
}

// StartSleeper waits for the child to enter SleepMain before inspecting its
// name or sending signals. The caller owns the running command and must reap it.
func StartSleeper(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if err := r.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	cmd.Args = append(cmd.Args, "--ready")
	cmd.Stdout = w
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = w.Close() // An early child exit must unblock the read.
	var ready [1]byte
	if _, err := r.Read(ready[:]); err != nil || ready[0] != 'R' {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("sleeper readiness: byte=%q err=%v", ready[0], err)
	}
}

// NamedSleeper exposes the test binary under the name seen by /proc.
func NamedSleeper(t *testing.T, dir, name string) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.Symlink(exe, path); err != nil {
		t.Fatal(err)
	}
	return path
}
