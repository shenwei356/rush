package main

import (
	"bytes"
	"context"
	"crypto/sha256"
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
	case "retry", "retry-output":
		path := os.Getenv("RUSH_LEGACY_ATTEMPTS")
		data, _ := os.ReadFile(path)
		attempt := bytes.Count(data, []byte("x\n")) + 1
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			os.Exit(20)
		}
		_, _ = f.WriteString("x\n")
		_ = f.Close()
		if os.Getenv("RUSH_LEGACY_HELPER_MODE") == "retry-output" {
			fmt.Printf("attempt %d\n", attempt)
		}
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

func TestLegacyAssignmentsPreserveValues(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
		want string
	}{
		{"multiline", []string{"-v", "\"p=first\nsecond\""}, "echo first\nsecond:{q}\n"},
		{"leading-newline", []string{"-v", "\"p=\nsecond\""}, "echo \nsecond:{q}\n"},
		{"comma-and-quotes", []string{"-v", `"p=a,b""c"`}, "echo a,b\"c:{q}\n"},
		{"multiple", []string{"-v", "p=one,q=two"}, "echo one:two\n"},
		{"repeated", []string{"-v", "p=one", "-v", "q=two", "-v", "p=last"}, "echo last:two\n"},
		{"equals", []string{"-v", "p=a=b"}, "echo a=b:{q}\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			args := append([]string{"--dry-run"}, test.args...)
			args = append(args, "echo {p}:{q}")
			stdout, stderr, code := runLegacyRush(t, "x\n", nil, args...)
			if code != 0 || stdout != test.want || stderr != "" {
				t.Fatalf("exit=%d stdout=%q stderr=%q; want %q", code, stdout, stderr, test.want)
			}
		})
	}
}

func TestLegacyAssignmentsRejectInvalidNames(t *testing.T) {
	for _, assignment := range []string{"1p=value", "a-b=value", "_p=value", "=p=value", "p"} {
		t.Run(assignment, func(t *testing.T) {
			stdout, stderr, code := runLegacyRush(t, "x\n", nil, "--dry-run", "-v", assignment, "echo {p}")
			if code == 0 || stdout != "" || !strings.Contains(stderr, "illegal value for flag -v/--assign") {
				t.Fatalf("invalid assignment accepted: exit=%d stdout=%q stderr=%q", code, stdout, stderr)
			}
		})
	}
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

func TestLegacyNRecordsLargerThanInput(t *testing.T) {
	batch := strconv.Itoa(int(^uint(0) >> 1))
	for _, input := range []string{"", "a\nb\nc\n"} {
		stdout, stderr, code := runLegacyRush(t, input, nil, "-n", batch, "-J", ",", "echo {}")
		want := ""
		if input != "" {
			want = "a,b,c"
		}
		if code != 0 || normalizedLines(stdout) != want {
			t.Fatalf("batching %q: exit=%d stdout=%q stderr=%q", input, code, stdout, stderr)
		}
	}
	helper := shellQuote(os.Args[0]) + " -test.run=^TestLegacyCommandHelper$"
	env := []string{"RUSH_LEGACY_COMMAND_HELPER=1", "RUSH_LEGACY_HELPER_MODE=stdin-framed"}
	stdout, stderr, code := runLegacyRush(t, "a\nb\nc", env, "--pipe", "-n", batch, helper)
	if code != 0 || strings.TrimSpace(stdout) != `"a\nb\nc"` {
		t.Fatalf("pipe batching: exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestLegacyDryRunWithHugeJobCount(t *testing.T) {
	var input, want strings.Builder
	for i := 0; i < 2200; i++ {
		fmt.Fprintf(&input, "%d\n", i)
		fmt.Fprintf(&want, "echo %d\n", i)
	}
	jobs := strconv.Itoa(int(^uint(0) >> 1))
	stdout, stderr, code := runLegacyRush(t, input.String(), nil, "--dry-run", "-k", "-j", jobs, "echo {}")
	if code != 0 || strings.ReplaceAll(stdout, "\r\n", "\n") != want.String() {
		t.Fatalf("large dry run: exit=%d output bytes=%d stderr=%q", code, len(stdout), stderr)
	}
}

func TestLegacyStopOnErrorKeepsFinalOutput(t *testing.T) {
	command := "echo evidence; exit 7"
	if runtime.GOOS == "windows" {
		command = "echo evidence & exit /b 7"
	}
	stdout, stderr, code := runLegacyRush(t, "x\n", nil, "-e", "-r", "2", "--print-retry-output=false", command)
	if code != 7 || normalizedLines(stdout) != "evidence" {
		t.Fatalf("final failed output: exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestLegacyRetryOutputSelection(t *testing.T) {
	for _, all := range []bool{false, true} {
		t.Run(strconv.FormatBool(all), func(t *testing.T) {
			helper := shellQuote(os.Args[0]) + " -test.run=^TestLegacyCommandHelper$"
			env := []string{"RUSH_LEGACY_COMMAND_HELPER=1", "RUSH_LEGACY_HELPER_MODE=retry-output", "RUSH_LEGACY_ATTEMPTS=" + t.TempDir() + "/attempts"}
			stdout, stderr, code := runLegacyRush(t, "x\n", env, "-r", "2", "--print-retry-output="+strconv.FormatBool(all), helper)
			want := "attempt 3\nretried\n"
			if all {
				want = "attempt 1\nattempt 2\n" + want
			}
			if code != 0 || strings.ReplaceAll(stdout, "\r\n", "\n") != want {
				t.Fatalf("retry output: exit=%d stdout=%q stderr=%q", code, stdout, stderr)
			}
		})
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

func TestPipePreservesLineBreakRecordsWithPlaceholders(t *testing.T) {
	helper := shellQuote(os.Args[0]) + " -test.run=^TestLegacyCommandHelper$ -- {#}"
	env := []string{"RUSH_LEGACY_COMMAND_HELPER=1", "RUSH_LEGACY_HELPER_MODE=stdin-framed"}
	for _, record := range []string{"\n", "\r", "\r\n"} {
		t.Run(strconv.Quote(record), func(t *testing.T) {
			input := record + "|"
			journal := t.TempDir() + "/successful.rush"
			args := []string{"--pipe", "-D", "|", "-c", "-C", journal, helper}
			stdout, stderr, code := runLegacyRush(t, input, env, args...)
			if code != 0 || stdout != strconv.Quote(input)+"\n" {
				t.Fatalf("line-break record lost: exit=%d stdout=%q stderr=%q", code, stdout, stderr)
			}
			stdout, stderr, code = runLegacyRush(t, input, env, args...)
			if code != 0 || stdout != "" {
				t.Fatalf("successful job reran: exit=%d stdout=%q stderr=%q", code, stdout, stderr)
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

func TestLegacyPipeTrimsRecordContents(t *testing.T) {
	helper := shellQuote(os.Args[0]) + " -test.run=^TestLegacyCommandHelper$"
	env := []string{"RUSH_LEGACY_COMMAND_HELPER=1", "RUSH_LEGACY_HELPER_MODE=stdin-framed"}
	for _, test := range []struct{ name, input, delimiter, trim, want string }{
		{"unchanged by default", " \ta \t\n \tb ", "\n", "", " \ta \t\n \tb "},
		{"left", " \ta \t\n \tb ", "\n", "l", "a \t\nb "},
		{"right", " \ta \t\n \tb ", "\n", "r", " \ta\n \tb"},
		{"both", " \ta \t\n \tb ", "\n", "b", "a\nb"},
		{"empty after trim", "  \n x \n", "\n", "b", "\nx\n"},
		{"custom delimiter", " aa || bb ", "||", "b", "aa||bb"},
		{"partial batch", " a ", "\n", "b", "a"},
	} {
		t.Run(test.name, func(t *testing.T) {
			stdout, stderr, code := runLegacyRush(t, test.input, env, "--pipe", "-n", "2", "-D", test.delimiter, "-T", test.trim, helper)
			if code != 0 || strings.TrimSpace(stdout) != strconv.Quote(test.want) {
				t.Fatalf("trimmed pipe input: exit=%d stdout=%q stderr=%q; want %q", code, stdout, stderr, test.want)
			}
		})
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

func TestLegacyDryRunDoesNotRecordSuccess(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(strconv.FormatBool(existing), func(t *testing.T) {
			journal := t.TempDir() + "/successful.rush"
			original := "echo old" + endMarkOfCMD
			if existing {
				if err := os.WriteFile(journal, []byte(original), 0600); err != nil {
					t.Fatal(err)
				}
			}
			args := []string{"-k", "-c", "-C", journal, "echo {}"}
			stdout, stderr, code := runLegacyRush(t, "old\nnew\n", nil, append([]string{"--dry-run"}, args...)...)
			want := "echo old\necho new\n"
			if existing {
				want = "echo new\n"
			}
			if code != 0 || strings.ReplaceAll(stdout, "\r\n", "\n") != want {
				t.Fatalf("preview: exit=%d stdout=%q stderr=%q", code, stdout, stderr)
			}
			data, err := os.ReadFile(journal)
			if existing && (err != nil || string(data) != original) {
				t.Fatalf("preview changed journal: %q, err=%v", data, err)
			}
			if !existing && !os.IsNotExist(err) {
				t.Fatalf("preview created journal: %q, err=%v", data, err)
			}
			stdout, stderr, code = runLegacyRush(t, "new\n", nil, args...)
			if code != 0 || normalizedLines(stdout) != "new" {
				t.Fatalf("real execution skipped previewed command: exit=%d stdout=%q stderr=%q", code, stdout, stderr)
			}
		})
	}
}

func TestLegacyContinueRejectsIncompleteRecord(t *testing.T) {
	journal := t.TempDir() + "/successful.rush"
	original := "echo old" + endMarkOfCMD + "echo new"
	if err := os.WriteFile(journal, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code := runLegacyRush(t, "new\n", nil, "-c", "-C", journal, "echo {}")
	if code == 0 || stdout != "" || !strings.Contains(stderr, "incomplete successful-command record") {
		t.Fatalf("incomplete record accepted: exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	data, err := os.ReadFile(journal)
	if err != nil || string(data) != original {
		t.Fatalf("invalid journal was modified: %q, err=%v", data, err)
	}
}

func TestContinuePreservesBracesAfterInputReplacement(t *testing.T) {
	for _, test := range []struct {
		name, input, want string
		args              []string
	}{
		{"field", "a{b}c\n", "b}c", []string{"-d", `\{`, "echo {2}"}},
		{"macro", "a{1}c\n", "a{1}c", []string{"-v", "p=echo {}", "{p}"}},
		{"adjacent macro", "}\n", "{p}", []string{"-v", "p=changed", "echo {p{}"}},
		{"adjacent fields", "{,}\n", "{p}", []string{"-d", ",", "-v", "p=changed", "echo {1}p{2}"}},
		{"adjacent job ID text", "{\n", "{#}", []string{"echo {}#}"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			args := append([]string{"-c", "-C", t.TempDir() + "/success"}, test.args...)
			stdout, stderr, code := runLegacyRush(t, test.input, nil, args...)
			if code != 0 || strings.TrimSpace(stdout) != test.want {
				t.Fatalf("stdout=%q stderr=%q exit=%d; want %q", stdout, stderr, code, test.want)
			}
			stdout, stderr, code = runLegacyRush(t, test.input, nil, args...)
			if code != 0 || stdout != "" {
				t.Fatalf("resume reran a completed command: stdout=%q stderr=%q exit=%d", stdout, stderr, code)
			}
		})
	}
}

func TestContinuePreservesNonSuffixText(t *testing.T) {
	journal := t.TempDir() + "/successful.rush"
	args := []string{"-c", "-C", journal, "echo {^.txt}"}
	for _, input := range []string{"sample.txt.bak", "sample.txt.other"} {
		stdout, stderr, code := runLegacyRush(t, input+"\n", nil, args...)
		if code != 0 || normalizedLines(stdout) != input {
			t.Fatalf("non-suffix text lost or job skipped: exit=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
	}
	stdout, stderr, code := runLegacyRush(t, "sample.txt.bak\nsample.txt.other\n", nil, args...)
	if code != 0 || stdout != "" {
		t.Fatalf("completed commands were not skipped: exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestLegacyContinueCommandContainingRecordDelimiter(t *testing.T) {
	journal := t.TempDir() + "/successful.rush"
	command := "printf first; # " + endMarkOfCMD
	if runtime.GOOS == "windows" {
		command = "echo first & REM " + endMarkOfCMD
	}
	args := []string{"-c", "-C", journal, command}
	stdout, stderr, code := runLegacyRush(t, "x\n", nil, args...)
	if code != 0 || normalizedLines(stdout) != "first" {
		t.Fatalf("first execution: exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	stdout, stderr, code = runLegacyRush(t, "x\n", nil, args...)
	if code != 0 || stdout != "" {
		t.Fatalf("delimiter split a successful command: exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestMixedExitStatuses(t *testing.T) {
	for _, test := range []struct {
		name, input     string
		propagate, stop bool
		want            int
	}{
		{"disabled later 124", "7\n124\n0\n", false, false, 124},
		{"disabled earlier 124", "124\n7\n", false, false, 124},
		{"disabled ordinary failures", "7\n8\n0\n", false, false, 0},
		{"enabled first failure", "7\n124\n", true, false, 7},
		{"enabled earlier 124", "124\n7\n", true, false, 124},
		{"stop overrides disabled propagation", "7\n124\n", false, true, 7},
	} {
		t.Run(test.name, func(t *testing.T) {
			args := []string{"-j", "1", "--propagate-exit-status=" + strconv.FormatBool(test.propagate), "--stop-on-error=" + strconv.FormatBool(test.stop), "exit {}"}
			stdout, stderr, code := runLegacyRush(t, test.input, nil, args...)
			if code != test.want || stdout != "" {
				t.Fatalf("exit=%d; want %d; stdout=%q stderr=%q", code, test.want, stdout, stderr)
			}
		})
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

func TestContinueDistinguishesLiteralPlaceholders(t *testing.T) {
	for _, placeholder := range []string{"{#}", "{?}"} {
		t.Run(placeholder, func(t *testing.T) {
			journal := t.TempDir() + "/success"
			command := "echo {1}" + placeholder + "{2}"
			args := []string{"-j", "1", "-c", "-C", journal, command}
			for _, input := range []string{"A" + placeholder + " B\n", "A " + placeholder + "B\n"} {
				want, baselineErr, baselineCode := runLegacyRush(t, input, nil, "-j", "1", command)
				stdout, stderr, code := runLegacyRush(t, input, nil, args...)
				if baselineCode != 0 || code != 0 || stdout != want || stderr != "" {
					t.Fatalf("input=%q exit=%d stdout=%q stderr=%q; baseline exit=%d stdout=%q stderr=%q", input, code, stdout, stderr, baselineCode, want, baselineErr)
				}
				stdout, stderr, code = runLegacyRush(t, input, nil, args...)
				if code != 0 || stdout != "" || stderr != "" {
					t.Fatalf("completed command was rerun: exit=%d stdout=%q stderr=%q", code, stdout, stderr)
				}
			}
		})
	}
}

func TestContinueRejectsAmbiguousLegacyRecords(t *testing.T) {
	for _, record := range []string{"echo {#}:a", `echo {\#}:a`, "echo {?}:a", "cat\n# rush --pipe stdin sha256: " + strings.Repeat("a", 64),
		encodeSuccessfulCommand(continueCommandPrefix + "echo \x00RUSH_CONTINUE_THREADS\x00"),
		encodeSuccessfulCommand(pipeContinueKey("echo \x00RUSH_CONTINUE_THREADS\x00", "a\n")),
		encodeSuccessfulCommand(continueCommandPrefix + "echo " + continueJobIDMarker),
		encodeSuccessfulCommand(pipeContinueKey("echo "+continueJobIDMarker, "a\n")),
	} {
		t.Run(record, func(t *testing.T) {
			journal, output := t.TempDir()+"/success", t.TempDir()+"/output"
			original := record + endMarkOfCMD
			if err := os.WriteFile(journal, []byte(original), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(output, []byte("preserved"), 0600); err != nil {
				t.Fatal(err)
			}
			stdout, stderr, code := runLegacyRush(t, "a\n", nil, "-c", "-C", journal, "-o", output, "echo {#}:{}")
			if code == 0 || stdout != "" || !strings.Contains(stderr, "ambiguous legacy successful-command record") {
				t.Fatalf("legacy ambiguity accepted: exit=%d stdout=%q stderr=%q", code, stdout, stderr)
			}
			for path, want := range map[string]string{journal: original, output: "preserved"} {
				data, err := os.ReadFile(path)
				if err != nil || string(data) != want {
					t.Fatalf("file changed: %s: %q, err=%v", path, data, err)
				}
			}
		})
	}
}

func TestContinueDistinguishesInvalidJobPlaceholders(t *testing.T) {
	for _, mode := range []string{"direct", "macro", "pipe"} {
		t.Run(mode, func(t *testing.T) {
			journal := t.TempDir() + "/success"
			threads := strconv.Itoa(runtime.NumCPU())
			run := func(expression string, jobs string) (string, string, int) {
				args := []string{"-j", jobs, "-c", "-C", journal}
				if mode == "macro" {
					args = append(args, "-v", "p="+expression)
					expression = "{p}"
				} else if mode == "pipe" {
					args = append(args, "--pipe")
				}
				return runLegacyRush(t, "x\n", nil, append(args, "echo \"begin"+expression+"end\"")...)
			}
			for _, test := range []struct{ expression, want string }{
				{"{?^" + threads + "}", "begin{?^" + threads + "}end"}, {"{?}", "begin" + threads + "end"},
				{"{?@([A-Z]+)}", "begin{?@([A-Z]+)}end"}, {"RUSH", "beginRUSHend"},
			} {
				stdout, stderr, code := run(test.expression, "1")
				if code != 0 || normalizedLines(stdout) != test.want {
					t.Fatalf("%s: exit=%d stdout=%q stderr=%q; want %q", test.expression, code, stdout, stderr, test.want)
				}
				stdout, stderr, code = run(test.expression, strconv.Itoa(runtime.NumCPU()+1))
				if code != 0 || stdout != "" {
					t.Fatalf("completed %s reran: exit=%d stdout=%q stderr=%q", test.expression, code, stdout, stderr)
				}
			}
		})
	}
}

func TestContinueEscapingPreservesMetadata(t *testing.T) {
	journal := t.TempDir() + "/success"
	command := "echo {#}:{?}:{}"
	args := []string{"-j", "1", "-n", "2", "-c", "-C", journal}
	firstArgs := append(append([]string{}, args...), "-q", "-Q", "R", command)
	stdout, stderr, code := runLegacyRush(t, "a\n", nil, firstArgs...)
	if code != 0 || normalizedLines(stdout) != fmt.Sprintf("1:%d:a", runtime.NumCPU()) || stderr != "" {
		t.Fatalf("first run: exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	stdout, stderr, code = runLegacyRush(t, "a\n", nil, append(args, command)...)
	if code != 0 || stdout != "" || stderr != "" {
		t.Fatalf("escaping changed only metadata but caused a rerun: exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestContinueSeparatesPipeMetadataFromLiteralComments(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("this reproduction uses Unix shell comments")
	}
	journal := t.TempDir() + "/success"
	args := []string{"-c", "-C", journal}
	stdout, stderr, code := runLegacyRush(t, "a\n", nil, append(append([]string{}, args...), "--pipe", "echo done")...)
	if code != 0 || stdout != "done\n" || stderr != "" {
		t.Fatalf("pipe run: exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	// This is the exact text used by the old pipe key, but also a valid command.
	digest := sha256.Sum256([]byte("a\n"))
	command := fmt.Sprintf("echo done\n# rush --pipe stdin sha256: %x", digest)
	stdout, stderr, code = runLegacyRush(t, "a\n", nil, append(args, command)...)
	if code != 0 || stdout != "done\n" || stderr != "" {
		t.Fatalf("literal comment matched pipe metadata: exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	stdout, stderr, code = runLegacyRush(t, "a\n", nil, append(args, command)...)
	if code != 0 || stdout != "" || stderr != "" {
		t.Fatalf("literal command was rerun: exit=%d stdout=%q stderr=%q", code, stdout, stderr)
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
	wantRecord := strconv.Quote(continueCommandPrefix + "echo " + continueJobIDMarker + ":a")[1:] + endMarkOfCMD
	if !strings.Contains(string(recorded), wantRecord) || !strings.Contains(string(recorded), "rush-continue-job-v1:") || strings.Contains(string(recorded), "echo 1:a") {
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
			for _, wantKey := range []string{"echo " + continueThreadsMarker + ":a", "echo " + continueThreadsMarker + ":b"} {
				if !strings.Contains(string(recorded), quotedCommandPrefix+strconv.Quote(continueCommandPrefix+wantKey)+endMarkOfCMD) {
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
	wantRecord := strconv.Quote(continueCommandPrefix + "echo " + continueJobIDMarker + ":" + continueThreadsMarker + ":a")[1:] + endMarkOfCMD
	if !strings.HasSuffix(string(recorded), wantRecord) || !strings.Contains(string(recorded), "rush-continue-job-v1:") {
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

func TestContinueComposedJobPlaceholders(t *testing.T) {
	for _, pipe := range []bool{false, true} {
		t.Run(fmt.Sprintf("pipe=%t", pipe), func(t *testing.T) {
			journal := t.TempDir() + "/success"
			args := []string{"-k", "-j", "1", "-c", "-C", journal, "-v", "p=}"}
			if pipe {
				args = append(args, "--pipe")
			}
			// The first job ID remains a printed value. The second becomes a
			// regexp operand on the next macro pass and changes the result.
			args = append(args, "echo {#}:{@({#}){p}")
			for _, step := range []struct{ input, want string }{
				{"12\nx\n", "1:1\n2:x"},
				{"x\n12\n", "2:2"},
				{"x\n12\n", ""},
			} {
				stdout, stderr, code := runLegacyRush(t, step.input, nil, args...)
				if code != 0 || normalizedLines(stdout) != step.want {
					t.Fatalf("input=%q: exit=%d stdout=%q stderr=%q; want %q", step.input, code, stdout, stderr, step.want)
				}
			}
		})
	}
	for _, placeholder := range []string{"#", "?"} {
		t.Run("field-"+placeholder, func(t *testing.T) {
			journal := t.TempDir() + "/success"
			args := []string{"-j", strconv.Itoa(runtime.NumCPU()), "-c", "-C", journal, "-v", "p=}", "echo {{" + placeholder + "}{p}"}
			for i, input := range []string{"alpha", "beta", "beta"} {
				stdout, stderr, code := runLegacyRush(t, input+"\n", nil, args...)
				want := input
				if i == 2 {
					want = ""
				}
				if code != 0 || normalizedLines(stdout) != want {
					t.Fatalf("input=%q: exit=%d stdout=%q stderr=%q; want %q", input, code, stdout, stderr, want)
				}
			}
		})
	}
}

func TestContinueRejectsOlderComposedJobRecords(t *testing.T) {
	for _, record := range []string{"echo alpha", continueCommandPrefix + "echo {" + continueJobIDMarker + "}"} {
		t.Run(record, func(t *testing.T) {
			journal, output := t.TempDir()+"/success", t.TempDir()+"/output"
			original := legacyQuotedCommandPrefix + strconv.Quote(record) + endMarkOfCMD
			if err := os.WriteFile(journal, []byte(original), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(output, []byte("preserved"), 0600); err != nil {
				t.Fatal(err)
			}
			stdout, stderr, code := runLegacyRush(t, "alpha\n", nil, "-c", "-C", journal, "-o", output, "-v", "p=}", "echo {{#}{p}")
			if code == 0 || stdout != "" || !strings.Contains(stderr, "ambiguous") {
				t.Errorf("older composed record accepted: exit=%d stdout=%q stderr=%q", code, stdout, stderr)
			}
			for path, want := range map[string]string{journal: original, output: "preserved"} {
				data, err := os.ReadFile(path)
				if err != nil || string(data) != want {
					t.Errorf("file changed: %q: %q, %v", path, data, err)
				}
			}
		})
	}
}

func TestContinueMixedComposedJobRecords(t *testing.T) {
	for _, older := range []bool{false, true} {
		t.Run(fmt.Sprintf("older=%t", older), func(t *testing.T) {
			dir := t.TempDir()
			journal, output := dir+"/success", dir+"/output"
			original := "echo old" + endMarkOfCMD
			if older {
				if err := os.WriteFile(journal, []byte(original), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(output, []byte("preserved"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			args := []string{"-k", "-j", "1", "-c", "-C", journal, "-o", output, "-v", "p=}", "echo {{#}{1}{p}"}
			for attempt := 0; attempt < 2; attempt++ {
				_, stderr, code := runLegacyRush(t, "{\nabc\n", nil, args...)
				if older {
					if code == 0 || !strings.Contains(stderr, "ambiguous") {
						t.Errorf("older records accepted: exit=%d stderr=%q", code, stderr)
					}
					for path, want := range map[string]string{journal: original, output: "preserved"} {
						data, err := os.ReadFile(path)
						if err != nil || string(data) != want {
							t.Errorf("file changed before ambiguous later job was rejected: %q: %q, %v", path, data, err)
						}
					}
					return
				}
				if code != 0 {
					t.Fatalf("new mixed records rejected: attempt=%d exit=%d stderr=%q", attempt, code, stderr)
				}
				want := "{1{}\n{2abc}"
				if attempt == 1 {
					want = ""
				}
				data, err := os.ReadFile(output)
				if err != nil || normalizedLines(string(data)) != want {
					t.Fatalf("output=%q err=%v; want %q", data, err, want)
				}
			}
		})
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
