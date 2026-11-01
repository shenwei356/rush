package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestOutputPreservedOnInitialCommandExpansionFailure(t *testing.T) {
	for _, test := range []struct {
		name, input string
		args        []string
	}{
		{"full batch", "bad=extra\n", nil},
		{"partial batch", "bad=extra\n", []string{"-n", "2"}},
		{"preceding success skipped", "solo\nbad=extra\n", []string{"-c"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			output, journal := dir+"/output", dir+"/success"
			command := "echo '{2@[}'"
			originalRecord := command + endMarkOfCMD
			if err := os.WriteFile(output, []byte("previous output"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(journal, []byte(originalRecord), 0600); err != nil {
				t.Fatal(err)
			}
			args := append([]string{"-d", "=", "-o", output, "-C", journal}, test.args...)
			stdout, stderr, code := runLegacyRush(t, test.input, nil, append(args, command)...)
			if code != 1 || stdout != "" || !strings.Contains(stderr, "fill command") {
				t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout, stderr)
			}
			for path, want := range map[string]string{output: "previous output", journal: originalRecord} {
				data, err := os.ReadFile(path)
				if err != nil || string(data) != want {
					t.Fatalf("file was changed: %s: %q, err=%v", path, data, err)
				}
			}
		})
	}
}

func TestOutputClearedWhenNoJobsRun(t *testing.T) {
	for _, input := range []string{"", "old\n"} {
		t.Run(fmt.Sprintf("input-%d", len(input)), func(t *testing.T) {
			dir := t.TempDir()
			output, journal := dir+"/output", dir+"/success"
			if err := os.WriteFile(output, []byte("previous output"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(journal, []byte("echo old"+endMarkOfCMD), 0600); err != nil {
				t.Fatal(err)
			}
			_, stderr, code := runLegacyRush(t, input, nil, "-c", "-C", journal, "-o", output, "echo {}")
			data, err := os.ReadFile(output)
			if code != 0 || err != nil || len(data) != 0 {
				t.Fatalf("exit=%d output=%q err=%v stderr=%q", code, data, err, stderr)
			}
		})
	}
}

func TestOutputRejectsInputAliases(t *testing.T) {
	for _, alias := range []string{"same", "hardlink", "symlink"} {
		for _, output := range []string{"output", "continue"} {
			t.Run(alias+"/"+output, func(t *testing.T) {
				dir := t.TempDir()
				input := filepath.Join(dir, "input")
				original := []byte("alpha\nbeta\n")
				if err := os.WriteFile(input, original, 0600); err != nil {
					t.Fatal(err)
				}
				target := input
				if alias != "same" {
					target = filepath.Join(dir, "alias")
					var err error
					if alias == "hardlink" {
						err = os.Link(input, target)
					} else {
						err = os.Symlink(input, target)
					}
					if err != nil {
						if runtime.GOOS == "windows" && alias == "symlink" {
							t.Skipf("symlink creation unavailable: %v", err)
						}
						t.Fatal(err)
					}
				}
				args := []string{"-i", input}
				if output == "output" {
					args = append(args, "-o", target)
				} else {
					args = append(args, "-c", "-C", target)
				}
				_, stderr, code := runLegacyRush(t, "", nil, append(args, "echo {}")...)
				if code == 0 || !strings.Contains(stderr, "same file") {
					t.Fatalf("exit=%d stderr=%q; want alias rejection", code, stderr)
				}
				got, err := os.ReadFile(input)
				if err != nil || !bytes.Equal(got, original) {
					t.Fatalf("input was changed: %q, err=%v", got, err)
				}
			})
		}
	}
}

func TestOutputRejectsContinueFileAlias(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "new", true: "existing"}[existing], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "result")
			original := []byte("echo old__CMD__\n")
			if existing {
				if err := os.WriteFile(path, original, 0600); err != nil {
					t.Fatal(err)
				}
			}
			_, stderr, code := runLegacyRush(t, "x\n", nil, "-o", path, "-c", "-C", path, "echo {}")
			if code == 0 || !strings.Contains(stderr, "same file") {
				t.Fatalf("exit=%d stderr=%q; want alias rejection", code, stderr)
			}
			got, err := os.ReadFile(path)
			if existing && (err != nil || !bytes.Equal(got, original)) {
				t.Fatalf("continue file was changed: %q, err=%v", got, err)
			}
			if !existing && !os.IsNotExist(err) {
				t.Fatalf("invalid output was created: %v", err)
			}
		})
	}
}

func TestOutputRejectsRedirectedInputAlias(t *testing.T) {
	for _, stdoutAlias := range []bool{false, true} {
		t.Run(map[bool]string{false: "named-output", true: "stdout"}[stdoutAlias], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "input")
			original := []byte("alpha\nbeta\n")
			if err := os.WriteFile(path, original, 0600); err != nil {
				t.Fatal(err)
			}
			input, err := os.OpenFile(path, os.O_RDWR, 0600)
			if err != nil {
				t.Fatal(err)
			}
			defer input.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			args := []string{"-test.run=^TestRushHelperProcess$", "--"}
			if !stdoutAlias {
				args = append(args, "-o", path)
			}
			cmd := exec.CommandContext(ctx, os.Args[0], append(args, "echo {}")...)
			cmd.Env = append(os.Environ(), "RUSH_TEST_HELPER_PROCESS=1")
			cmd.Stdin = input
			if stdoutAlias {
				cmd.Stdout = input
			}
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			err = cmd.Run()
			if ctx.Err() != nil || err == nil || !strings.Contains(stderr.String(), "same file") {
				t.Fatalf("redirected input was not rejected: %v, %s", err, &stderr)
			}
			got, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(got, original) {
				t.Fatalf("redirected input changed: %q, err=%v", got, err)
			}
		})
	}
}

func TestDiagnosticsRejectProtectedFileAliases(t *testing.T) {
	for _, role := range []string{"input", "stdin", "continue", "invalid-options", "parse-before", "parse-after", "unknown-before", "syntax-before", "parse-before-continue", "invalid-continue"} {
		for _, sharedStdout := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/shared-stdout=%t", role, sharedStdout), func(t *testing.T) {
				dir := t.TempDir()
				path, output := dir+"/protected", dir+"/output"
				original := "alpha\n"
				resume := strings.Contains(role, "continue")
				if resume {
					original = encodeSuccessfulCommand("echo previous") + endMarkOfCMD
				}
				if err := os.WriteFile(path, []byte(original), 0600); err != nil {
					t.Fatal(err)
				}
				redirected, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0600)
				if err != nil {
					t.Fatal(err)
				}
				defer redirected.Close()
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				args := []string{"-test.run=^TestRushHelperProcess$", "--", "--verbose", "-o", output}
				switch role {
				case "parse-before", "parse-before-continue":
					args = append(args, "--jobs=invalid")
				case "unknown-before":
					args = append(args, "--unknown-flag")
				case "syntax-before":
					args = append(args, "---invalid")
				}
				if role != "stdin" && !resume {
					args = append(args, "-i", path)
				} else if resume {
					args = append(args, "-c", "-C", path)
				}
				if role == "invalid-options" {
					args = append(args, "--trim", "invalid")
				}
				if role == "parse-after" {
					args = append(args, "--jobs=invalid")
				}
				if role == "invalid-continue" {
					args = append(args, "--continue=invalid")
				}
				cmd := exec.CommandContext(ctx, os.Args[0], append(args, "echo {}")...)
				cmd.Env = append(os.Environ(), "RUSH_TEST_HELPER_PROCESS=1")
				cmd.Stdin = strings.NewReader("alpha\n")
				if role == "stdin" {
					input, err := os.Open(path)
					if err != nil {
						t.Fatal(err)
					}
					defer input.Close()
					cmd.Stdin = input
				}
				var diagnostic bytes.Buffer
				cmd.Stdout = &diagnostic
				if sharedStdout {
					cmd.Stdout = redirected
				}
				cmd.Stderr = redirected
				runErr := cmd.Run()
				if ctx.Err() != nil || runErr == nil {
					t.Errorf("diagnostic alias accepted: err=%v stdout=%q", runErr, &diagnostic)
				}
				if data, err := os.ReadFile(path); err != nil || string(data) != original {
					t.Errorf("protected file changed: %.200q, err=%v", data, err)
				}
				if _, err := os.Stat(output); !os.IsNotExist(err) {
					t.Errorf("output created before alias rejection: %v", err)
				}
				if !sharedStdout && !strings.Contains(diagnostic.String(), "stderr and") {
					t.Errorf("missing safe diagnostic: %q", &diagnostic)
				}
			})
		}
	}
}

func TestOutputRejectsNewContinueAliasBeforeCreation(t *testing.T) {
	dir := t.TempDir()
	output := filepath.Join(dir, "output")
	alias := filepath.Join(dir, "alias")
	if err := os.Symlink(output, alias); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlink creation unavailable: %v", err)
		}
		t.Fatal(err)
	}
	_, stderr, code := runLegacyRush(t, "x\n", nil, "-o", output, "-c", "-C", alias, "echo {}")
	if code == 0 || !strings.Contains(stderr, "same file") {
		t.Fatalf("new alias accepted: exit=%d stderr=%q", code, stderr)
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatalf("created output before checking new aliases: %v", err)
	}
}

func TestOutputOverwriteAndDeviceStreams(t *testing.T) {
	path := filepath.Join(t.TempDir(), "output")
	if err := os.WriteFile(path, []byte("old output with a longer tail\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_, stderr, code := runLegacyRush(t, "x\n", nil, "-o", path, "echo {}")
	data, err := os.ReadFile(path)
	if code != 0 || err != nil || normalizedLines(string(data)) != "x" {
		t.Fatalf("overwrite: exit=%d data=%q err=%v stderr=%q", code, data, err, stderr)
	}
	_, stderr, code = runLegacyRush(t, "", nil, "-i", os.DevNull, "-o", os.DevNull, "echo {}")
	if code != 0 {
		t.Fatalf("device input/output rejected: exit=%d stderr=%q", code, stderr)
	}
}

func TestOutputPreflightPreservesExistingFiles(t *testing.T) {
	for _, invalid := range []string{"input", "continue-parent", "continue-directory"} {
		t.Run(invalid, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "output")
			original := []byte("keep this output\n")
			if err := os.WriteFile(path, original, 0600); err != nil {
				t.Fatal(err)
			}
			args := []string{"-o", path}
			switch invalid {
			case "input":
				args = append(args, "-i", filepath.Join(dir, "missing"))
			case "continue-parent":
				args = append(args, "-c", "-C", filepath.Join(dir, "missing", "record"))
			case "continue-directory":
				args = append(args, "-c", "-C", dir)
			}
			_, stderr, code := runLegacyRush(t, "x\n", nil, append(args, "echo {}")...)
			if code == 0 {
				t.Fatalf("invalid path accepted: %q", stderr)
			}
			got, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(got, original) {
				t.Fatalf("output was changed before validation: %q, err=%v", got, err)
			}
		})
	}
}

func TestOutputCloseFailuresAreReported(t *testing.T) {
	for _, sink := range []string{"output", "journal"} {
		for _, mode := range []string{"normal", "no-status-propagation", "empty-input"} {
			t.Run(fmt.Sprintf("%s/%s", sink, mode), func(t *testing.T) {
				dir := t.TempDir()
				output, journal := filepath.Join(dir, "output"), filepath.Join(dir, "journal")
				original := []byte("echo old" + endMarkOfCMD)
				if err := os.WriteFile(journal, original, 0600); err != nil {
					t.Fatal(err)
				}
				faultPath := output
				if sink == "journal" {
					faultPath = journal
				}
				input := "x\n"
				args := []string{"-o", output, "-c", "-C", journal}
				if mode == "no-status-propagation" {
					args = append(args, "--propagate-exit-status=false")
				} else if mode == "empty-input" {
					input = ""
				}
				_, stderr, code := runLegacyRush(t, input, []string{"RUSH_TEST_CLOSE_ERROR_FILE=" + faultPath}, append(args, "echo {}")...)
				if code != 1 || !strings.Contains(stderr, "injected close failure") {
					t.Fatalf("close failure lost: exit=%d stderr=%q", code, stderr)
				}
				if sink == "output" {
					data, err := os.ReadFile(journal)
					if err != nil || !bytes.Equal(data, original) {
						t.Fatalf("failed output retained success records: %q, err=%v", data, err)
					}
				}
			})
		}
	}
}

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
