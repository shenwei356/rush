package main

import (
	"bufio"
	"bytes"
	"errors"
	"os"
	"runtime"
	"testing"
)

func withSuccessfulCommandSeams(t *testing.T) {
	t.Helper()
	oldWrite, oldFlush := writeSuccessfulCommandFile, flushSuccessfulCommandFile
	oldTruncate, oldSeek := truncateSuccessfulCommandFile, seekSuccessfulCommandFile
	t.Cleanup(func() {
		writeSuccessfulCommandFile, flushSuccessfulCommandFile = oldWrite, oldFlush
		truncateSuccessfulCommandFile, seekSuccessfulCommandFile = oldTruncate, oldSeek
	})
}

func TestOutputSuccessfulCommandFileFailures(t *testing.T) {
	sentinel := errors.New("successful-command fault")
	t.Run("write", func(t *testing.T) {
		withSuccessfulCommandSeams(t)
		writeSuccessfulCommandFile = func(*bufio.Writer, string) (int, error) { return 0, sentinel }
		if err := appendSuccessfulCommand(bufio.NewWriter(os.Stdout), "echo x"); !errors.Is(err, sentinel) {
			t.Fatalf("error=%v", err)
		}
	})
	t.Run("flush", func(t *testing.T) {
		withSuccessfulCommandSeams(t)
		flushSuccessfulCommandFile = func(*bufio.Writer) error { return sentinel }
		if err := appendSuccessfulCommand(bufio.NewWriter(os.Stdout), "echo x"); !errors.Is(err, sentinel) {
			t.Fatalf("error=%v", err)
		}
	})
	for _, name := range []string{"truncate", "seek"} {
		t.Run(name, func(t *testing.T) {
			withSuccessfulCommandSeams(t)
			f, err := os.CreateTemp(t.TempDir(), "successful")
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if name == "truncate" {
				truncateSuccessfulCommandFile = func(*os.File, int64) error { return sentinel }
			} else {
				seekSuccessfulCommandFile = func(*os.File, int64, int) (int64, error) { return 0, sentinel }
			}
			if err := rollbackSuccessfulCommands(bufio.NewWriter(f), f, 0); !errors.Is(err, sentinel) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestBufferedOutputFailureCommandHelper(t *testing.T) {
	mode := ""
	for i, arg := range os.Args {
		if arg == "rush-buffered-output-helper" && i+1 < len(os.Args) {
			mode = os.Args[i+1]
			break
		}
	}
	if mode == "" || mode == "success" {
		return
	}
	if mode != "large" {
		t.Fatalf("unknown helper mode %q", mode)
	}
	_, _ = os.Stdout.Write(bytes.Repeat([]byte("x"), (1<<20)+(32<<10)))
}

func TestSuccessfulCommandFileRollsBackOnBufferedOutputFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows: os.CreateTemp uses GetTempPath which has multiple fallbacks, making it hard to force failure")
	}
	dir := t.TempDir()
	successFile := dir + string(os.PathSeparator) + "successful.rush"
	original := []byte("already complete__CMD__\n")
	if err := os.WriteFile(successFile, original, 0600); err != nil {
		t.Fatal(err)
	}
	missingTemp := dir + string(os.PathSeparator) + "missing-temp"
	t.Setenv("TMPDIR", missingTemp)
	t.Setenv("TMP", missingTemp)
	t.Setenv("TEMP", missingTemp)
	helper := shellQuote(os.Args[0]) + " -test.run=^TestBufferedOutputFailureCommandHelper$ -- rush-buffered-output-helper {}"
	_, stderr, code := runLegacyRush(t, "success\nlarge\n", nil, "-j", "1", "-c", "-C", successFile, helper)
	if code != 1 {
		t.Fatalf("exit code=%d; want 1; stderr=%q", code, stderr)
	}
	got, err := os.ReadFile(successFile)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("successful-command file=%q; want original %q", got, original)
	}
}
