//go:build darwin

package process

import (
	"os"
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
