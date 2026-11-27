// Package testutil provides subprocess fixtures for Rush tests.
package testutil

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// SleepMain lets a renamed test binary act as a quiet, single-process sleeper.
func SleepMain(m *testing.M) {
	if len(os.Args) == 2 {
		if duration, ok := strings.CutPrefix(os.Args[1], "--test-sleep="); ok {
			delay, err := time.ParseDuration(duration)
			if err != nil {
				os.Exit(2)
			}
			time.Sleep(delay)
			os.Exit(0)
		}
	}
	os.Exit(m.Run())
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
