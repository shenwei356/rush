//go:build darwin

package process

import (
	"context"
	"os"
	"strings"
	"testing"
)

func TestDarwinProcessIdentityMatchesSnapshot(t *testing.T) {
	pid := os.Getpid()
	first, err := lookupPlatformProcess(pid)
	if err != nil {
		t.Fatal(err)
	}
	all, err := snapshotPlatformProcesses()
	if err != nil {
		t.Fatal(err)
	}
	fromSnapshot, ok := all[pid]
	if !ok || fromSnapshot.identity != first.identity || fromSnapshot.pgid != first.pgid {
		t.Fatalf("snapshot process=%+v, found=%t; lookup=%+v", fromSnapshot, ok, first)
	}
	again, err := lookupPlatformProcess(pid)
	if err != nil {
		t.Fatal(err)
	}
	if again.identity != first.identity {
		t.Fatalf("process identity changed from %d to %d", first.identity, again.identity)
	}
}

func TestDarwinFullNameUsedForExclusions(t *testing.T) {
	p, err := lookupPlatformProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	p.name = strings.Repeat("n", 16)
	old := darwinProcessName
	darwinProcessName = func(context.Context, int) (string, error) {
		return "/usr/local/bin/long-executable-name", nil
	}
	t.Cleanup(func() { darwinProcessName = old })
	name, err := fullPlatformProcessName(p)
	if err != nil {
		t.Fatal(err)
	}
	if allowed, _ := canSendSignal(name, []string{"long-executable-name"}); allowed {
		t.Fatalf("full executable name %q did not match exclusion", name)
	}
}
