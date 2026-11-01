package main

import (
	"os"
	"strings"
	"testing"
)

func TestJobContinueKeyInputIdentity(t *testing.T) {
	command := continueCommandPrefix + "echo " + continueJobIDMarker
	key := func(records []string, trim string) string {
		return jobContinueKey(command, records, trim, make(continueJobOccurrences))
	}
	batch := key([]string{"a", "b"}, "")
	if batch == key([]string{"a\nb"}, "") || batch == key([]string{"ab"}, "") {
		t.Fatal("record boundaries were lost")
	}
	if batch != key([]string{" a ", " b "}, "b") {
		t.Fatal("trimming changed the identity of equivalent input")
	}
	if continueJobCommand(batch) != command || !validContinueJobKey(batch) {
		t.Fatal("key lost the normalized command")
	}
	counts := make(continueJobOccurrences)
	first := jobContinueKey(command, []string{"a"}, "", counts)
	other := jobContinueKey(command, []string{"b"}, "", counts)
	second := jobContinueKey(command, []string{"a"}, "", counts)
	if first == second {
		t.Fatal("duplicate batches share a key")
	}
	counts = make(continueJobOccurrences)
	if jobContinueKey(command, []string{"b"}, "", counts) != other ||
		jobContinueKey(command, []string{"a"}, "", counts) != first ||
		jobContinueKey(command, []string{"a"}, "", counts) != second {
		t.Fatal("reordering distinct batches changed duplicate identities")
	}
	if got := jobContinueKey("echo plain", []string{"a"}, "", nil); got != "echo plain" {
		t.Fatal("ordinary command matching changed")
	}
}

func TestContinueRejectsDamagedJobIdentity(t *testing.T) {
	command := continueCommandPrefix + "echo " + continueJobIDMarker
	for _, suffix := range []string{
		"incomplete", strings.Repeat("a", 64) + ":0\x00" + command,
		strings.Repeat("z", 64) + ":1\x00" + command,
		strings.Repeat("a", 64) + ":1\x00echo plain",
	} {
		t.Run(suffix, func(t *testing.T) {
			dir := t.TempDir()
			journal, output := dir+"/records", dir+"/output"
			original := encodeSuccessfulCommand(continueJobPrefix+suffix) + endMarkOfCMD
			if err := os.WriteFile(journal, []byte(original), 0600); err != nil {
				t.Fatal(err)
			}
			stdout, stderr, code := runLegacyRush(t, "a\n", nil, "-c", "-C", journal, "-o", output, "echo {#}")
			if code == 0 || stdout != "" || !strings.Contains(stderr, "invalid job identity") {
				t.Fatalf("damaged identity accepted: exit=%d stdout=%q stderr=%q", code, stdout, stderr)
			}
			if data, err := os.ReadFile(journal); err != nil || string(data) != original {
				t.Fatalf("record file changed: %q, err=%v", data, err)
			}
			if _, err := os.Stat(output); !os.IsNotExist(err) {
				t.Fatalf("output created: %v", err)
			}
		})
	}
}
