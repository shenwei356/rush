package main

import (
	"os"
	"strings"
	"testing"
)

func TestMultiValueFlagsRejectExtraCSVRecords(t *testing.T) {
	for _, flag := range []string{"--assign", "--infile", "--no-stop-exes", "--no-kill-exes"} {
		t.Run(flag, func(t *testing.T) {
			value := "p=one\np=two"
			if flag == "--infile" {
				first, second := t.TempDir()+"/first", t.TempDir()+"/second"
				for _, path := range []string{first, second} {
					if err := os.WriteFile(path, []byte("record\n"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				value = first + "\n" + second
			}
			stdout, stderr, code := runLegacyRush(t, "x\n", nil, "--dry-run", flag, value, "echo {}")
			if code == 0 || stdout != "" || !strings.Contains(stderr, "single CSV record") {
				t.Fatalf("extra CSV record accepted: exit=%d stdout=%q stderr=%q", code, stdout, stderr)
			}
		})
	}
}
