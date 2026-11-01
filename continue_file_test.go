package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestContinueFileRollbackPreservesOriginalRecords(t *testing.T) {
	journal := t.TempDir() + "/records"
	original := encodeSuccessfulCommand("echo old") + endMarkOfCMD
	if err := os.WriteFile(journal, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := openContinueFile(journal, false)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	records, _ := readSuccCmdsFrom(f)
	if _, ok := records["echo old"]; !ok {
		t.Fatal("locked handle did not read original record")
	}
	w := bufio.NewWriter(f)
	if err := appendSuccessfulCommand(w, "echo failed-output"); err != nil {
		t.Fatal(err)
	}
	if err := rollbackSuccessfulCommands(w, f, int64(len(original))); err != nil {
		t.Fatalf("record handle cannot roll back: %v", err)
	}
	if err := appendSuccessfulCommand(w, "echo resumed"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	want := original + encodeSuccessfulCommand("echo resumed") + endMarkOfCMD
	if data, err := os.ReadFile(journal); err != nil || string(data) != want {
		t.Fatalf("rollback damaged records: %q, err=%v; want %q", data, err, want)
	}
}

func TestContinueLockRejectsConflictsBeforeOutput(t *testing.T) {
	for _, mode := range []struct {
		name            string
		reader, preview bool
	}{
		{"writer-writer", false, false},
		{"writer-preview", false, true},
		{"reader-writer", true, false},
	} {
		for _, existing := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/existing-output=%t", mode.name, existing), func(t *testing.T) {
				dir := t.TempDir()
				journal, output := dir+"/records", dir+"/output"
				original := encodeSuccessfulCommand("echo old") + endMarkOfCMD
				if err := os.WriteFile(journal, []byte(original), 0600); err != nil {
					t.Fatal(err)
				}
				if existing {
					if err := os.WriteFile(output, []byte("previous output"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				owner, err := openContinueFile(journal, mode.reader)
				if err != nil {
					t.Fatal(err)
				}
				defer owner.Close()
				args := []string{"-c", "-C", journal, "-o", output}
				if mode.preview {
					args = append(args, "--dry-run")
				}
				stdout, stderr, code := runLegacyRush(t, "new\n", nil, append(args, "echo {}")...)
				if code == 0 || stdout != "" || !strings.Contains(stderr, "lock successful-command file") {
					t.Fatalf("conflicting run accepted: exit=%d stdout=%q stderr=%q", code, stdout, stderr)
				}
				if err := owner.Close(); err != nil {
					t.Fatal(err)
				}
				if data, err := os.ReadFile(journal); err != nil || string(data) != original {
					t.Fatalf("records changed: %q, err=%v", data, err)
				}
				data, err := os.ReadFile(output)
				if existing {
					if err != nil || string(data) != "previous output" {
						t.Fatalf("output changed: %q, err=%v", data, err)
					}
				} else if !os.IsNotExist(err) {
					t.Fatalf("output created: %q, err=%v", data, err)
				}
				stdout, stderr, code = runLegacyRush(t, "new\n", nil, "-c", "-C", journal, "echo {}")
				if code != 0 || strings.ReplaceAll(stdout, "\r\n", "\n") != "new\n" {
					t.Fatalf("lock survived close: exit=%d stdout=%q stderr=%q", code, stdout, stderr)
				}
			})
		}
	}
}

func TestContinuePreviewsShareReadLock(t *testing.T) {
	journal := t.TempDir() + "/records"
	original := encodeSuccessfulCommand("echo old") + endMarkOfCMD
	if err := os.WriteFile(journal, []byte(original), 0400); err != nil {
		t.Fatal(err)
	}
	owner, err := openContinueFile(journal, true)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	stdout, stderr, code := runLegacyRush(t, "old\nnew\n", nil, "-c", "-C", journal, "--dry-run", "echo {}")
	if code != 0 || stdout != "echo new\n" {
		t.Fatalf("read-only preview failed: exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if data, err := os.ReadFile(journal); err != nil || string(data) != original {
		t.Fatalf("preview modified records: %q, err=%v", data, err)
	}
}

func TestContinueLockHeldUntilProcessExit(t *testing.T) {
	journal := t.TempDir() + "/records"
	if err := os.WriteFile(journal, nil, 0600); err != nil {
		t.Fatal(err)
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRushHelperProcess$", "--", "-c", "-C", journal, "echo {}")
	cmd.Env = append(os.Environ(), "RUSH_TEST_HELPER_PROCESS=1")
	// Keep input open without sending records. The process must already hold
	// the record lock while waiting, before it has run any commands.
	cmd.Stdin = reader
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	deadline := time.Now().Add(2 * time.Second)
	for {
		probe, err := openContinueFile(journal, false)
		if err != nil {
			if !strings.Contains(err.Error(), "lock successful-command file") {
				t.Fatal(err)
			}
			break
		}
		_ = probe.Close()
		if time.Now().After(deadline) {
			t.Fatal("waiting rush did not hold the record lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	stdout, diagnostic, code := runLegacyRush(t, "new\n", nil, "-c", "-C", journal, "echo {}")
	if code == 0 || stdout != "" || !strings.Contains(diagnostic, "lock successful-command file") {
		t.Fatalf("concurrent writer accepted: exit=%d stdout=%q stderr=%q", code, stdout, diagnostic)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	stdout, diagnostic, code = runLegacyRush(t, "new\n", nil, "-c", "-C", journal, "echo {}")
	if code != 0 || strings.ReplaceAll(stdout, "\r\n", "\n") != "new\n" {
		t.Fatalf("lock survived process exit: exit=%d stdout=%q stderr=%q; first stderr=%q", code, stdout, diagnostic, stderr.String())
	}
}
