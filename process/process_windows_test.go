//go:build windows

package process

import (
	"context"
	"os"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestWindowsExclusionsDoNotSignalOrKill(t *testing.T) {
	opts := &Options{NoStopExes: []string{"all"}, NoKillExes: []string{"all"}}
	controller, err := newPlatformProcessController(opts)
	if err != nil {
		t.Fatal(err)
	}
	cmd := getCommand(context.Background(), "ping -n 30 127.0.0.1 >NUL")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := controller.Started(cmd); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		controller.Finished(cmd)
		_ = controller.Close()
	}()
	if err := controller.StopAll(0, nil); err != nil {
		t.Fatal(err)
	}
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(cmd.Process.Pid))
	if err != nil {
		t.Fatalf("excluded process was killed: %v", err)
	}
	defer windows.CloseHandle(h)
	status, err := windows.WaitForSingleObject(h, 0)
	if err != nil || status != uint32(windows.WAIT_TIMEOUT) {
		t.Fatalf("excluded process is not alive: status=%d err=%v", status, err)
	}
}

func TestWindowsProcessHandlesReturnToBaseline(t *testing.T) {
	runWindowsHandleCycle(t)
	baseline := currentProcessHandleCount(t)
	for i := 0; i < 10; i++ {
		runWindowsHandleCycle(t)
	}
	// Allow some tolerance for handle cleanup timing on Windows CI
	tolerance := uint32(20)
	got := currentProcessHandleCount(t)
	if got > baseline+tolerance {
		t.Fatalf("process handle count=%d; want baseline %d (with tolerance +%d)", got, baseline, tolerance)
	}
}

func runWindowsHandleCycle(t *testing.T) {
	t.Helper()
	input := make(chan string, 1)
	input <- "echo ok"
	close(input)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opts := &Options{Jobs: 1, OutFileHandle: os.Stdout, ErrFileHandle: os.Stderr, PropExitStatus: true}
	out, success, done, statuses := Run4OutputContext(opts, ctx, cancel, input)
	for range out {
	}
	for range success {
	}
	for range statuses {
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("run did not finish")
	}
}

func currentProcessHandleCount(t *testing.T) uint32 {
	t.Helper()
	process, err := windows.GetCurrentProcess()
	if err != nil {
		t.Fatal(err)
	}
	var count uint32
	proc := windows.NewLazySystemDLL("kernel32.dll").NewProc("GetProcessHandleCount")
	ok, _, callErr := proc.Call(uintptr(process), uintptr(unsafe.Pointer(&count)))
	if ok == 0 {
		t.Fatalf("GetProcessHandleCount: %v", callErr)
	}
	return count
}
