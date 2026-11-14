//go:build linux

package process

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"
	"time"
)

func TestHasProcessNameLongNamesAndZombies(t *testing.T) {
	name := fmt.Sprintf("rush-name-%x-worker", time.Now().UnixNano())
	path := filepath.Join(t.TempDir(), name)
	data, err := os.ReadFile("/bin/sleep")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(path, "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	for _, test := range []struct {
		name string
		want bool
	}{{name, true}, {name[:15], false}, {name + "extra", false}} {
		got, err := HasProcessName(context.Background(), test.name)
		if err != nil || got != test.want {
			t.Fatalf("HasProcessName(%q)=%t, %v; want %t", test.name, got, err, test.want)
		}
	}
	for _, test := range []struct {
		pattern string
		want    bool
	}{
		{regexp.QuoteMeta(name[15:]) + "$", true},
		{"^" + regexp.QuoteMeta(name[:15]) + "$", false},
	} {
		matcher, err := NewNameMatcher(test.pattern, true)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := matcher.Exists(context.Background()); err != nil || got != test.want {
			t.Fatalf("pattern %q: found=%t, err=%v; want %t", test.pattern, got, err, test.want)
		}
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		p, err := readLinuxProcess(cmd.Process.Pid)
		if err != nil {
			t.Fatal(err)
		}
		if p.zombie {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child did not become a zombie")
		}
		time.Sleep(time.Millisecond)
	}
	if got, err := HasProcessName(context.Background(), name); err != nil || got {
		t.Fatalf("zombie counted as a running process: %t, %v", got, err)
	}
}
