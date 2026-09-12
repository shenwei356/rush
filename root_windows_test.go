//go:build windows

package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

var (
	kernel32                  = windows.NewLazySystemDLL("kernel32.dll")
	procAttachConsole         = kernel32.NewProc("AttachConsole")
	procFreeConsole           = kernel32.NewProc("FreeConsole")
	procSetConsoleCtrlHandler = kernel32.NewProc("SetConsoleCtrlHandler")
)

func TestWindowsConsoleSenderHelper(t *testing.T) {
	pidText := os.Getenv("RUSH_WINDOWS_ATTACH_PID")
	if pidText == "" {
		return
	}
	pid, err := strconv.ParseUint(pidText, 10, 32)
	if err != nil {
		windowsHelperExit("parse attach pid", err)
	}
	_, _, _ = procFreeConsole.Call()
	if ok, _, callErr := procAttachConsole.Call(uintptr(pid)); ok == 0 {
		windowsHelperExit("AttachConsole", callErr)
	}
	defer procFreeConsole.Call()
	if ok, _, callErr := procSetConsoleCtrlHandler.Call(0, 1); ok == 0 {
		windowsHelperExit("ignore console control events", callErr)
	}
	if err := windows.GenerateConsoleCtrlEvent(windows.CTRL_C_EVENT, 0); err != nil {
		windowsHelperExit("GenerateConsoleCtrlEvent(CTRL_C_EVENT)", err)
	}
	os.Exit(0)
}

func TestWindowsConsoleChildHelper(t *testing.T) {
	readyName := os.Getenv("RUSH_WINDOWS_READY_EVENT")
	if readyName == "" {
		return
	}
	breakEvent := openWindowsEvent(os.Getenv("RUSH_WINDOWS_BREAK_EVENT"))
	defer windows.CloseHandle(breakEvent)
	readyEvent := openWindowsEvent(readyName)
	defer windows.CloseHandle(readyEvent)
	holdEvent := openWindowsEvent(os.Getenv("RUSH_WINDOWS_HOLD_EVENT"))
	defer windows.CloseHandle(holdEvent)

	callback := syscall.NewCallback(func(event uint32) uintptr {
		if event == windows.CTRL_BREAK_EVENT {
			_ = windows.SetEvent(breakEvent)
		}
		if event == windows.CTRL_C_EVENT || event == windows.CTRL_BREAK_EVENT {
			return 1
		}
		return 0
	})
	if ok, _, err := procSetConsoleCtrlHandler.Call(callback, 1); ok == 0 {
		windowsHelperExit("SetConsoleCtrlHandler", err)
	}
	if err := os.WriteFile(os.Getenv("RUSH_WINDOWS_CHILD_PID"), []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
		windowsHelperExit("write child pid", err)
	}
	if err := windows.SetEvent(readyEvent); err != nil {
		windowsHelperExit("set ready event", err)
	}
	_, _ = windows.WaitForSingleObject(holdEvent, windows.INFINITE)
	os.Exit(0)
}

func TestWindowsCtrlCStopsTreeAndQueue(t *testing.T) {
	runWindowsConsoleLifecycle(t, time.Second, false)
}

func TestWindowsSecondCtrlCForcesExit(t *testing.T) {
	runWindowsConsoleLifecycle(t, 30*time.Second, true)
}

func runWindowsConsoleLifecycle(t *testing.T, cleanup time.Duration, second bool) {
	t.Helper()
	readyName, ready := newWindowsEvent(t, "ready")
	breakName, breakEvent := newWindowsEvent(t, "break")
	holdName, _ := newWindowsEvent(t, "hold")
	dir := t.TempDir()
	pidFile := dir + `\child.pid`
	queuedFile := dir + `\queued`
	helperCommand := shellQuote(os.Args[0]) + " -test.run=^TestWindowsConsoleChildHelper$"
	input := helperCommand + "\n" + `echo queued>"` + queuedFile + `"` + "\n"
	args := []string{
		"-test.run=^TestRushHelperProcess$", "--", "-j", "1",
		"--cleanup-time", strconv.Itoa(int(cleanup / time.Second)), "{}",
	}
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(),
		"RUSH_TEST_HELPER_PROCESS=1",
		"RUSH_WINDOWS_READY_EVENT="+readyName,
		"RUSH_WINDOWS_BREAK_EVENT="+breakName,
		"RUSH_WINDOWS_HOLD_EVENT="+holdName,
		"RUSH_WINDOWS_CHILD_PID="+pidFile,
	)
	cmd.Stdin = strings.NewReader(input)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_CONSOLE}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Give more time on CI for process startup
	waitWindowsEvent(t, ready, 15*time.Second, "child ready")
	childPID := readWindowsPID(t, pidFile)
	t.Cleanup(func() { terminateWindowsPID(childPID) })

	sendWindowsCtrlC(t, cmd.Process.Pid)
	waitWindowsEvent(t, breakEvent, 8*time.Second, "directed CTRL_BREAK_EVENT")
	forcedAt := time.Time{}
	if second {
		forcedAt = time.Now()
		sendWindowsCtrlC(t, cmd.Process.Pid)
	}
	waitWindowsCommand(t, cmd, 10*time.Second, &stderr)
	if code := cmd.ProcessState.ExitCode(); code != 130 {
		t.Fatalf("exit code=%d; want 130\nstdout=%s\nstderr=%s", code, stdout.String(), stderr.String())
	}
	if second && time.Since(forcedAt) > 5*time.Second {
		t.Fatalf("second control event did not bypass %s cleanup delay", cleanup)
	}
	if _, err := os.Stat(queuedFile); !os.IsNotExist(err) {
		t.Fatalf("queued command ran: %v", err)
	}
	if windowsPIDAlive(childPID) {
		t.Fatalf("ordinary descendant %d is still alive", childPID)
	}
}

func sendWindowsCtrlC(t *testing.T, pid int) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestWindowsConsoleSenderHelper$")
	cmd.Env = append(os.Environ(), fmt.Sprintf("RUSH_WINDOWS_ATTACH_PID=%d", pid))
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("console sender: %v\n%s", err, output)
	}
}

func newWindowsEvent(t *testing.T, suffix string) (string, windows.Handle) {
	t.Helper()
	name := fmt.Sprintf(`Local\rush-%d-%d-%s`, os.Getpid(), time.Now().UnixNano(), suffix)
	p, err := windows.UTF16PtrFromString(name)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateEvent(nil, 1, 0, p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = windows.CloseHandle(h) })
	return name, h
}

func openWindowsEvent(name string) windows.Handle {
	p, err := windows.UTF16PtrFromString(name)
	if err != nil {
		windowsHelperExit("event name", err)
	}
	h, err := windows.OpenEvent(windows.EVENT_MODIFY_STATE|windows.SYNCHRONIZE, false, p)
	if err != nil {
		windowsHelperExit("OpenEvent", err)
	}
	return h
}

func waitWindowsEvent(t *testing.T, event windows.Handle, timeout time.Duration, what string) {
	t.Helper()
	status, err := windows.WaitForSingleObject(event, uint32(timeout/time.Millisecond))
	if err != nil || status != uint32(windows.WAIT_OBJECT_0) {
		t.Fatalf("wait for %s: status=%d err=%v", what, status, err)
	}
}

func waitWindowsCommand(t *testing.T, cmd *exec.Cmd, timeout time.Duration, stderr *bytes.Buffer) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		<-done
		t.Fatalf("rush did not exit within %s; stderr=%s", timeout, stderr.String())
	}
}

func readWindowsPID(t *testing.T, path string) uint32 {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 32)
	if err != nil {
		t.Fatal(err)
	}
	return uint32(pid)
}

func windowsPIDAlive(pid uint32) bool {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, pid)
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	status, err := windows.WaitForSingleObject(h, 0)
	return err == nil && status == uint32(windows.WAIT_TIMEOUT)
}

func terminateWindowsPID(pid uint32) {
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, pid)
	if err == nil {
		_ = windows.TerminateProcess(h, 1)
		_ = windows.CloseHandle(h)
	}
}

func windowsHelperExit(action string, err error) {
	fmt.Fprintf(os.Stderr, "%s: %v\n", action, err)
	os.Exit(2)
}
