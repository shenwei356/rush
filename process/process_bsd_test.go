//go:build freebsd || openbsd

package process

import (
	"os"
	"testing"
)

func TestBSDProcessIdentityMatchesSnapshot(t *testing.T) {
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
	if err != nil || again.identity != first.identity {
		t.Fatalf("second lookup=%+v, err=%v; first=%+v", again, err, first)
	}
}
