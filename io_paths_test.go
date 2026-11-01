package main

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/pflag"
)

func FuzzDiagnosticConfigMatchesParsedFlags(f *testing.F) {
	for _, args := range [][]string{
		{},
		{"echo {}", "-j", "3", "-i", "a,b", "-i", `"c,d"`, "-c", "-C", "journal", "-o", "output"},
		{"-qkci\"a,b\"", "-Cjournal", "-ooutput"},
		{"-D", "--", "-i", "data"},
		{"--", "-i", "data", "-c"},
		{"-D", "-i", "not-an-input", "-v", "p=--infile=also-not-input"},
		{"-i="},
		{"-i\"\""},
		{"-i", "\"a\nb\",c", "-i", ""},
		{"--continue=false", "-i=a,b"},
		{"--infile="},
		{"-c=false", "--continue=true", "--succ-cmd-file=other", "--out-file=result"},
	} {
		f.Add(strings.Join(args, "\x00"))
	}
	f.Fuzz(func(t *testing.T, encoded string) {
		if len(encoded) > 4096 {
			return
		}
		args := strings.Split(encoded, "\x00")
		flags := pflag.NewFlagSet("files", pflag.ContinueOnError)
		flags.SetOutput(io.Discard)
		flags.StringSliceP("infile", "i", nil, "")
		flags.StringP("out-file", "o", "-", "")
		flags.StringP("succ-cmd-file", "C", "successful_cmds.rush", "")
		flags.BoolP("continue", "c", false, "")
		flags.BoolP("escape", "q", false, "")
		flags.BoolP("keep-order", "k", false, "")
		flags.IntP("jobs", "j", 1, "")
		flags.StringP("record-delimiter", "D", "\n", "")
		flags.StringSliceP("assign", "v", nil, "")
		// Match the CLI's rejection of extra, unquoted CSV records.
		inputFlag := flags.Lookup("infile")
		inputFlag.Value = &completeCSVValue{Value: inputFlag.Value, SliceValue: inputFlag.Value.(pflag.SliceValue)}
		got := diagnosticConfig(flags, args)
		if flags.Parsed() || flags.NFlag() != 0 {
			t.Fatal("diagnostic scan changed the real flags")
		}
		if err := flags.Parse(args); err != nil {
			return // Invalid options are covered by the CLI preservation tests.
		}
		inputs, _ := flags.GetStringSlice("infile")
		output, _ := flags.GetString("out-file")
		journal, _ := flags.GetString("succ-cmd-file")
		resume, _ := flags.GetBool("continue")
		if !slices.Equal(got.Infiles, inputs) || got.OutFile != output || got.SuccCmdFile != journal || got.Continue != resume {
			t.Fatalf("diagnostic file options differ from parsed options: got inputs=%q output=%q journal=%q continue=%t; want inputs=%q output=%q journal=%q continue=%t", got.Infiles, got.OutFile, got.SuccCmdFile, got.Continue, inputs, output, journal, resume)
		}
	})
}

func TestContinueRejectsDeviceRecordBeforeOutputCreation(t *testing.T) {
	for _, dryRun := range []bool{false, true} {
		for _, existingOutput := range []bool{false, true} {
			name := "run"
			if dryRun {
				name = "preview"
			}
			if existingOutput {
				name += "/existing-output"
			}
			t.Run(name, func(t *testing.T) {
				output := filepath.Join(t.TempDir(), "output")
				if existingOutput {
					if err := os.WriteFile(output, []byte("preserved"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				args := []string{"-c", "-C", os.DevNull, "-o", output}
				if dryRun {
					args = append(args, "--dry-run")
				}
				stdout, stderr, code := runLegacyRush(t, "x\n", nil, append(args, "echo {}")...)
				if code == 0 || stdout != "" || !strings.Contains(stderr, "successful-command file must be a regular file") {
					t.Errorf("device record accepted: exit=%d stdout=%q stderr=%q", code, stdout, stderr)
				}
				data, err := os.ReadFile(output)
				if existingOutput {
					if err != nil || string(data) != "preserved" {
						t.Errorf("existing output changed: %q, %v", data, err)
					}
				} else if !os.IsNotExist(err) {
					t.Errorf("output created before rejection: %q, %v", data, err)
				}
			})
		}
	}
	// A record path is irrelevant unless --continue is enabled. Ordinary input
	// and output devices remain supported.
	if err := validateRunFiles(Config{Infiles: []string{os.DevNull}, OutFile: os.DevNull, SuccCmdFile: os.DevNull}); err != nil {
		t.Fatalf("unused record path or ordinary device stream rejected: %v", err)
	}
}

func TestDanglingOutputSymlinkValidation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("tests Unix symlink resolution and creation without symlink privileges")
	}
	for _, mode := range []string{"output-alias", "journal-alias", "chained-alias", "distinct", "missing-parent"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			child := filepath.Join(dir, "real", "child")
			if err := os.MkdirAll(child, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(child, filepath.Join(dir, "directory-link")); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(dir, "file-link")
			target := filepath.Join(dir, "real", "result")
			output, journal := link, target
			// This relative target must be resolved by the filesystem; cleaning
			// directory-link/.. would incorrectly redirect it to dir/result.
			linkTarget := "directory-link/../result"
			switch mode {
			case "journal-alias":
				output, journal = target, link
				linkTarget = target // Also cover absolute symlink targets.
			case "chained-alias":
				if err := os.Symlink(linkTarget, filepath.Join(dir, "second-link")); err != nil {
					t.Fatal(err)
				}
				linkTarget = "second-link"
			case "distinct":
				journal = filepath.Join(dir, "result")
			case "missing-parent":
				output, journal = filepath.Join(dir, "output"), link
				linkTarget = "missing/record"
			}
			if err := os.Symlink(linkTarget, link); err != nil {
				t.Fatal(err)
			}
			_, stderr, code := runLegacyRush(t, "ok\n", nil, "-c", "-C", journal, "-o", output, "echo {}")
			if mode == "distinct" {
				if code != 0 {
					t.Fatalf("distinct targets rejected: exit=%d stderr=%q", code, stderr)
				}
				data, err := os.ReadFile(target)
				if err != nil || normalizedLines(string(data)) != "ok" {
					t.Fatalf("incorrect output: %q, %v", data, err)
				}
				data, err = os.ReadFile(journal)
				if err != nil || string(data) != encodeSuccessfulCommand("echo ok")+endMarkOfCMD {
					t.Fatalf("incorrect journal: %q, %v", data, err)
				}
				return
			}
			message := "same file"
			if mode == "missing-parent" {
				message = "successful-command file directory"
			}
			if code == 0 || !strings.Contains(stderr, message) {
				t.Errorf("invalid targets accepted or diagnosed late: exit=%d stderr=%q", code, stderr)
			}
			for _, path := range []string{output, journal, target} {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Errorf("created file before rejecting paths: %s: %v", path, err)
				}
			}
			if got, err := os.Readlink(link); err != nil || got != linkTarget {
				t.Errorf("original symlink modified: %q, %v", got, err)
			}
		})
	}
}

func TestNewOutputPathsThroughSymlinkParent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("tests Unix resolution of a symlink followed by ..")
	}
	for _, alias := range []bool{false, true} {
		t.Run(map[bool]string{false: "distinct", true: "same"}[alias], func(t *testing.T) {
			dir := t.TempDir()
			real := filepath.Join(dir, "real")
			if err := os.MkdirAll(filepath.Join(real, "child"), 0700); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(dir, "link")
			if err := os.Symlink(filepath.Join(real, "child"), link); err != nil {
				t.Fatal(err)
			}
			// Joining would erase the very symlink/.. spelling under test.
			output := link + "/../result"
			journal := filepath.Join(dir, "result")
			if alias {
				journal = filepath.Join(real, "result")
			}
			_, stderr, code := runLegacyRush(t, "ok\n", nil, "-c", "-C", journal, "-o", output, "echo {}")
			if alias {
				if code == 0 || !strings.Contains(stderr, "same file") {
					t.Fatalf("alias accepted: exit=%d stderr=%q", code, stderr)
				}
				if _, err := os.Stat(output); !os.IsNotExist(err) {
					t.Fatalf("created aliased output before rejection: %v", err)
				}
				return
			}
			if code != 0 {
				t.Fatalf("distinct paths rejected: exit=%d stderr=%q", code, stderr)
			}
			data, err := os.ReadFile(filepath.Join(real, "result"))
			if err != nil || normalizedLines(string(data)) != "ok" {
				t.Fatalf("incorrect output: %q, %v", data, err)
			}
			data, err = os.ReadFile(journal)
			if err != nil || string(data) != encodeSuccessfulCommand("echo ok")+endMarkOfCMD {
				t.Fatalf("incorrect journal: %q, %v", data, err)
			}
		})
	}
}

func TestInvalidSymlinkParentDoesNotCreateOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("tests Unix resolution of a symlink followed by ..")
	}
	dir := t.TempDir()
	child := filepath.Join(dir, "real", "child")
	if err := os.MkdirAll(child, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(child, link); err != nil {
		t.Fatal(err)
	}
	// The lexically cleaned parent exists; the real parent does not.
	if err := os.Mkdir(filepath.Join(dir, "missing"), 0700); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "output")
	journal := link + "/../missing/record"
	_, stderr, code := runLegacyRush(t, "ok\n", nil, "-o", output, "-c", "-C", journal, "echo {}")
	if code == 0 || !strings.Contains(stderr, "successful-command file directory") {
		t.Fatalf("invalid parent not detected during validation: exit=%d stderr=%q", code, stderr)
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatalf("created output before rejecting invalid journal parent: %v", err)
	}
}
