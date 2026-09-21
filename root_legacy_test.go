package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestLegacyCommandHelper(t *testing.T) {
	if os.Getenv("RUSH_LEGACY_COMMAND_HELPER") != "1" {
		return
	}
	switch os.Getenv("RUSH_LEGACY_HELPER_MODE") {
	case "retry":
		path := os.Getenv("RUSH_LEGACY_ATTEMPTS")
		data, _ := os.ReadFile(path)
		attempt := bytes.Count(data, []byte("x\n")) + 1
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			os.Exit(20)
		}
		_, _ = f.WriteString("x\n")
		_ = f.Close()
		if attempt < 3 {
			os.Exit(7)
		}
		fmt.Print("retried\n")
	case "timeout":
		time.Sleep(30 * time.Second)
	case "large":
		n, _ := strconv.Atoi(os.Getenv("RUSH_LEGACY_OUTPUT_BYTES"))
		fmt.Print(strings.Repeat("x", n))
	case "stdin-framed":
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			os.Exit(22)
		}
		fmt.Printf("%q\n", string(data))
	case "stdin-retry":
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			os.Exit(22)
		}
		path := os.Getenv("RUSH_LEGACY_ATTEMPTS")
		previous, _ := os.ReadFile(path)
		attempt := bytes.Count(previous, []byte{0}) + 1
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			os.Exit(20)
		}
		_, _ = f.Write(data)
		_, _ = f.Write([]byte{0})
		_ = f.Close()
		if attempt < 3 {
			os.Exit(7)
		}
		_, _ = os.Stdout.Write(data)
	default:
		os.Exit(21)
	}
	os.Exit(0)
}

func TestLegacyBasicOrderingAndNRecords(t *testing.T) {
	input := "1\n2\n3\n4\n5\n6\n"
	stdout, stderr, code := runLegacyRush(t, input, nil, "-j", "2", "echo", "{}")
	if code != 0 || stderr != "" {
		t.Fatalf("basic: code=%d stderr=%q", code, stderr)
	}
	assertLineSet(t, stdout, []string{"1", "2", "3", "4", "5", "6"})

	stdout, stderr, code = runLegacyRush(t, input, nil, "-j", "2", "-k", "echo", "{}")
	if code != 0 || stderr != "" || normalizedLines(stdout) != "1\n2\n3\n4\n5\n6" {
		t.Fatalf("ordered: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}

	stdout, stderr, code = runLegacyRush(t, input, nil, "-j", "1", "-n", "2", "echo", "{1}")
	if code != 0 || stderr != "" || normalizedLines(stdout) != "1\n3\n5" {
		t.Fatalf("nrecords: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestLegacyVerboseAndReplacementVariants(t *testing.T) {
	stdout, stderr, code := runLegacyRush(t, "a\nb\n", nil, "-j", "1", "--verbose", "echo", "{}")
	if code != 0 || normalizedLines(stdout) != "a\nb" || strings.Count(stderr, "start cmd") != 2 || strings.Count(stderr, "finish cmd") != 2 {
		t.Fatalf("verbose: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}

	stdout, stderr, code = runLegacyRush(t, "123 dir/file.txt.gz\n", nil, "-j", "1", "echo job {#} {1} {2} {2.} {2:} {2/} {2%.}")
	got := strings.Join(strings.Fields(stdout), "")
	// Normalize path separators for cross-platform comparison
	got = strings.ReplaceAll(got, string(os.PathSeparator), "/")
	if code != 0 || stderr != "" || got != "job1123dir/file.txt.gzdir/file.txtdir/filedirfile.txt" {
		t.Fatalf("replacement: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestLegacyDelimiterBoundaries(t *testing.T) {
	stdout, stderr, code := runLegacyRush(t, "aa,bb||cc,dd||tail,ee", nil, "-j", "1", "-D", "||", "-d", ",", "echo {1}:{2}")
	if code != 0 || stderr != "" || normalizedLines(stdout) != "aa:bb\ncc:dd\ntail:ee" {
		t.Fatalf("delimiter: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestLegacyPipeRecordBatches(t *testing.T) {
	helper := shellQuote(os.Args[0]) + " -test.run=^TestLegacyCommandHelper$"
	env := []string{"RUSH_LEGACY_COMMAND_HELPER=1", "RUSH_LEGACY_HELPER_MODE=stdin-framed"}
	tests := []struct {
		name  string
		input string
		args  []string
		want  string
	}{
		{name: "newline with unterminated final record", input: "a\nb\nc", args: []string{"-n", "2"}, want: "\"a\\nb\\n\"\n\"c\"\n"},
		{name: "custom delimiter", input: "aa||bb||cc", args: []string{"-D", "||", "-n", "2"}, want: "\"aa||bb||\"\n\"cc\"\n"},
		{name: "one byte records", input: "abc", args: []string{"-D", "", "-n", "2"}, want: "\"ab\"\n\"c\"\n"},
		{name: "empty records are ignored", input: "a\n\nb\n", args: []string{"-n", "2"}, want: "\"a\\nb\\n\"\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := append([]string{"--pipe", "-j", "1", "-k"}, tt.args...)
			args = append(args, helper)
			stdout, stderr, code := runLegacyRush(t, tt.input, env, args...)
			if code != 0 || stdout != tt.want || stderr != "" {
				t.Fatalf("code=%d stdout=%q stderr=%q; want %q", code, stdout, stderr, tt.want)
			}
		})
	}
}

func TestLegacyPipeReplaysStdinOnRetry(t *testing.T) {
	helper := shellQuote(os.Args[0]) + " -test.run=^TestLegacyCommandHelper$"
	attempts := t.TempDir() + string(os.PathSeparator) + "attempts"
	env := []string{"RUSH_LEGACY_COMMAND_HELPER=1", "RUSH_LEGACY_HELPER_MODE=stdin-retry", "RUSH_LEGACY_ATTEMPTS=" + attempts}
	input := "alpha\nbeta\n"
	stdout, stderr, code := runLegacyRush(t, input, env, "--pipe", "-j", "1", "-n", "2", "-r", "2", helper)
	data, err := os.ReadFile(attempts)
	if err != nil {
		t.Fatal(err)
	}
	wantAttempts := strings.Repeat(input+"\x00", 3)
	if code != 0 || stdout != input || string(data) != wantAttempts || strings.Count(stderr, "wait cmd") != 2 {
		t.Fatalf("code=%d stdout=%q attempts=%q stderr=%q", code, stdout, data, stderr)
	}
}

func TestLegacyPipeContinueIncludesStdin(t *testing.T) {
	helper := shellQuote(os.Args[0]) + " -test.run=^TestLegacyCommandHelper$"
	env := []string{"RUSH_LEGACY_COMMAND_HELPER=1", "RUSH_LEGACY_HELPER_MODE=stdin-framed"}
	successFile := t.TempDir() + string(os.PathSeparator) + "successful.rush"
	args := []string{"--pipe", "-j", "1", "-k", "-n", "2", "-c", "-C", successFile, helper}

	stdout, stderr, code := runLegacyRush(t, "a\nb\nc\n", env, args...)
	if code != 0 || stdout != "\"a\\nb\\n\"\n\"c\\n\"\n" || stderr != "" {
		t.Fatalf("first run: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	stdout, stderr, code = runLegacyRush(t, "a\nb\nc\n", env, args...)
	if code != 0 || stdout != "" || stderr != "" {
		t.Fatalf("second run: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	stdout, stderr, code = runLegacyRush(t, "a\nb\nx\n", env, args...)
	if code != 0 || stdout != "\"x\\n\"\n" || stderr != "" {
		t.Fatalf("changed input: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	recorded, err := os.ReadFile(successFile)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(recorded, []byte("# rush --pipe stdin sha256:")) {
		t.Fatalf("successful-command file does not identify piped input: %q", recorded)
	}
}

func TestLegacyRetryAndTimeout(t *testing.T) {
	helper := shellQuote(os.Args[0]) + " -test.run=^TestLegacyCommandHelper$"
	attempts := t.TempDir() + string(os.PathSeparator) + "attempts"
	env := []string{"RUSH_LEGACY_COMMAND_HELPER=1", "RUSH_LEGACY_HELPER_MODE=retry", "RUSH_LEGACY_ATTEMPTS=" + attempts}
	stdout, stderr, code := runLegacyRush(t, "x\n", env, "-j", "1", "-r", "2", helper)
	data, err := os.ReadFile(attempts)
	if err != nil {
		t.Fatalf("failed to read attempts file %q: %v\ncode=%d stdout=%q stderr=%q", attempts, err, code, stdout, stderr)
	}
	if code != 0 || normalizedLines(stdout) != "retried" || bytes.Count(data, []byte("x\n")) != 3 || strings.Count(stderr, "wait cmd") != 2 {
		t.Fatalf("retry: code=%d attempts=%q stdout=%q stderr=%q", code, data, stdout, stderr)
	}

	env = []string{"RUSH_LEGACY_COMMAND_HELPER=1", "RUSH_LEGACY_HELPER_MODE=timeout"}
	stdout, stderr, code = runLegacyRush(t, "x\n", env, "-j", "1", "-t", "1", "--cleanup-time", "0", helper)
	if code != 124 || stdout != "" || !strings.Contains(stderr, "time out") {
		t.Fatalf("timeout: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestLegacyContinueFileAndMultilineCommands(t *testing.T) {
	successFile := t.TempDir() + string(os.PathSeparator) + "successful.rush"
	// On Windows, multiline commands with \n in a string don't work the same way through cmd.exe
	// Use && for command chaining on Windows, \n on Unix
	var command string
	var expectedFirst string
	if runtime.GOOS == "windows" {
		command = "echo {} && echo s{}"
		// Windows echo adds \r\n, so we only check for the presence of expected parts
		expectedFirst = "1\n2" // Relaxed check for Windows
	} else {
		command = "echo {}\necho s{}"
		expectedFirst = "1\ns1\n2\ns2"
	}

	for run := 1; run <= 2; run++ {
		stdout, stderr, code := runLegacyRush(t, "1\n2\n", nil, "-j", "1", "-c", "-C", successFile, command)
		if code != 0 {
			t.Fatalf("run %d: code=%d stderr=%q", run, code, stderr)
		}
		if run == 1 {
			normalized := normalizedLines(stdout)
			if runtime.GOOS == "windows" {
				// On Windows, just check that we got some output
				if !strings.Contains(normalized, "1") || !strings.Contains(normalized, "2") {
					t.Fatalf("first run stdout=%q (normalized=%q)", stdout, normalized)
				}
			} else if normalized != expectedFirst {
				t.Fatalf("first run stdout=%q (normalized=%q, expected=%q)", stdout, normalized, expectedFirst)
			}
		}
		if run == 2 && (stdout != "" || stderr != "") {
			t.Fatalf("second run stdout=%q stderr=%q", stdout, stderr)
		}
	}
}

func TestLegacyContinueSkipLoggingIsVerboseOnly(t *testing.T) {
	successFile := t.TempDir() + string(os.PathSeparator) + "successful.rush"
	stdout, stderr, code := runLegacyRush(t, "a\nb\n", nil, "-j", "1", "-c", "-C", successFile, "echo {}")
	if code != 0 || normalizedLines(stdout) != "a\nb" || stderr != "" {
		t.Fatalf("first run: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}

	stdout, stderr, code = runLegacyRush(t, "a\nb\n", nil, "-j", "1", "-c", "-C", successFile, "echo {}")
	if code != 0 || stdout != "" || stderr != "" {
		t.Fatalf("default resume: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}

	stdout, stderr, code = runLegacyRush(t, "a\nb\n", nil, "-j", "1", "--verbose", "-c", "-C", successFile, "echo {}")
	if code != 0 || stdout != "" || strings.Count(stderr, "ignore cmd") != 2 {
		t.Fatalf("verbose resume: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestLegacyJobNumberPlaceholderWithContinue(t *testing.T) {
	successFile := t.TempDir() + string(os.PathSeparator) + "successful.rush"
	command := `echo {#}:{}`

	stdout, stderr, code := runLegacyRush(t, "a\nb\n", nil, "-j", "1", "-c", "-C", successFile, command)
	if code != 0 || normalizedLines(stdout) != "1:a\n2:b" || stderr != "" {
		t.Fatalf("first run: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	recorded, err := os.ReadFile(successFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(recorded), "echo {#}:a") || strings.Contains(string(recorded), "echo 1:a") {
		t.Fatalf("successful-command file contains unstable job number: %q", recorded)
	}

	stdout, stderr, code = runLegacyRush(t, "b\nc\na\n", nil, "-j", "1", "-c", "-C", successFile, command)
	if code != 0 || normalizedLines(stdout) != "2:c" || stderr != "" {
		t.Fatalf("resumed run: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestLegacyJobNumberPlaceholderReadsOldContinueFile(t *testing.T) {
	successFile := t.TempDir() + string(os.PathSeparator) + "successful.rush"
	if err := os.WriteFile(successFile, []byte("echo 1:a"+endMarkOfCMD), 0600); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code := runLegacyRush(t, "a\n", nil, "-j", "1", "-c", "-C", successFile, "echo {#}:{}")
	if code != 0 || stdout != "" || stderr != "" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestLegacyThreadsPlaceholderWithContinue(t *testing.T) {
	for _, tt := range []struct {
		name    string
		command string
		assign  []string
	}{
		{name: "direct", command: "echo {?}:{}"},
		{name: "preset variable", command: "echo {threads}:{}", assign: []string{"-v", "threads={?}"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			successFile := t.TempDir() + string(os.PathSeparator) + "successful.rush"
			args := append([]string{"-j", "1", "-c", "-C", successFile}, tt.assign...)
			args = append(args, tt.command)
			stdout, stderr, code := runLegacyRush(t, "a\nb\n", nil, args...)
			want := fmt.Sprintf("%d:a\n%d:b", runtime.NumCPU(), runtime.NumCPU())
			if code != 0 || normalizedLines(stdout) != want || stderr != "" {
				t.Fatalf("first run: code=%d stdout=%q stderr=%q; want %q", code, stdout, stderr, want)
			}

			recorded, err := os.ReadFile(successFile)
			if err != nil {
				t.Fatal(err)
			}
			for _, wantKey := range []string{"echo {?}:a", "echo {?}:b"} {
				if !strings.Contains(string(recorded), wantKey+endMarkOfCMD) {
					t.Fatalf("successful-command file does not contain %q: %q", wantKey, recorded)
				}
			}

			args = append([]string{"-j", strconv.Itoa(runtime.NumCPU() + 1), "-c", "-C", successFile}, tt.assign...)
			args = append(args, tt.command)
			stdout, stderr, code = runLegacyRush(t, "b\nc\na\n", nil, args...)
			if code != 0 || normalizedLines(stdout) != "1:c" || stderr != "" {
				t.Fatalf("resumed run: code=%d stdout=%q stderr=%q", code, stdout, stderr)
			}
		})
	}
}

func TestLegacyDynamicPlaceholdersWithContinue(t *testing.T) {
	successFile := t.TempDir() + string(os.PathSeparator) + "successful.rush"
	command := "echo {#}:{?}:{}"
	stdout, stderr, code := runLegacyRush(t, "a\n", nil, "-j", "1", "-c", "-C", successFile, command)
	if code != 0 || normalizedLines(stdout) != fmt.Sprintf("1:%d:a", runtime.NumCPU()) || stderr != "" {
		t.Fatalf("first run: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	recorded, err := os.ReadFile(successFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(recorded) != "echo {#}:{?}:a"+endMarkOfCMD {
		t.Fatalf("successful-command file=%q", recorded)
	}

	stdout, stderr, code = runLegacyRush(t, "b\na\n", nil, "-j", strconv.Itoa(runtime.NumCPU()+1), "-c", "-C", successFile, command)
	if code != 0 || normalizedLines(stdout) != "1:1:b" || stderr != "" {
		t.Fatalf("resumed run: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestLegacyThreadsPlaceholderReadsOldContinueFile(t *testing.T) {
	successFile := t.TempDir() + string(os.PathSeparator) + "successful.rush"
	oldRecord := fmt.Sprintf("echo %d:a", runtime.NumCPU()) + endMarkOfCMD
	if err := os.WriteFile(successFile, []byte(oldRecord), 0600); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code := runLegacyRush(t, "a\n", nil, "-j", "1", "-c", "-C", successFile, "echo {?}:{}")
	if code != 0 || stdout != "" || stderr != "" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestLegacyLargeBufferedOutput(t *testing.T) {
	const size = (1 << 20) + 33
	helper := shellQuote(os.Args[0]) + " -test.run=^TestLegacyCommandHelper$"
	env := []string{"RUSH_LEGACY_COMMAND_HELPER=1", "RUSH_LEGACY_HELPER_MODE=large", fmt.Sprintf("RUSH_LEGACY_OUTPUT_BYTES=%d", size)}
	for _, args := range [][]string{{"-j", "1", helper}, {"-j", "1", "-k", helper}} {
		stdout, stderr, code := runLegacyRush(t, "x\n", env, args...)
		if code != 0 || stderr != "" || len(stdout) != size {
			t.Fatalf("args=%v code=%d bytes=%d stderr=%q", args, code, len(stdout), stderr)
		}
	}
}

func runLegacyRush(t *testing.T, stdin string, extraEnv []string, args ...string) (string, string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	testArgs := append([]string{"-test.run=^TestRushHelperProcess$", "--"}, args...)
	cmd := exec.CommandContext(ctx, os.Args[0], testArgs...)
	cmd.Env = append(os.Environ(), "RUSH_TEST_HELPER_PROCESS=1")
	cmd.Env = append(cmd.Env, extraEnv...)
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("rush exceeded test deadline: %v", ctx.Err())
	}
	code := 0
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatal(err)
		}
		code = exitErr.ExitCode()
	}
	return stdout.String(), stderr.String(), code
}

func shellQuote(value string) string {
	if runtime.GOOS == "windows" {
		// On Windows, don't quote at all - the 8.3 short path format doesn't have spaces
		// and rush/cmd.exe will handle the path correctly without quotes
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func normalizedLines(value string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(value, "\r", "")), "\n")
}

func assertLineSet(t *testing.T, got string, want []string) {
	t.Helper()
	lines := strings.Fields(strings.ReplaceAll(got, "\r", ""))
	sort.Strings(lines)
	sort.Strings(want)
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("lines=%v; want %v", lines, want)
	}
}
