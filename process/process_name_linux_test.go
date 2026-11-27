package process

import (
	"context"
	"os"
	"strconv"
	"testing"
)

func TestLinuxProcessNameRejectsWrappedPID(t *testing.T) {
	if strconv.IntSize < 64 {
		t.Skip("requires a PID wider than int32")
	}
	// Narrowing this value would resolve the running test process.
	pid := int(int64(os.Getpid()) + 1<<32)
	if name, err := linuxProcessName(context.Background(), pid); err == nil {
		t.Fatalf("out-of-range pid %d resolved to %q", pid, name)
	}
}
