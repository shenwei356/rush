//go:build windows

package process

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestWindowsArgumentHelper(t *testing.T) {
	if os.Getenv("RUSH_WINDOWS_ARGUMENT_HELPER") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			if err := json.NewEncoder(os.Stdout).Encode(os.Args[i+1:]); err != nil {
				os.Exit(2)
			}
			os.Exit(0)
		}
	}
	os.Exit(2)
}

func TestWindowsCommandQuoting(t *testing.T) {
	for _, tc := range []struct{ command, output string }{
		{`echo "two words"`, "\"two words\"\r\n"},
		{`echo C:\path\`, "C:\\path\\\r\n"},
	} {
		cmd := getCommand(context.Background(), tc.command)
		output, err := cmd.CombinedOutput()
		if err != nil || string(output) != tc.output {
			t.Errorf("command=%q output=%q err=%v; want %q", tc.command, output, err, tc.output)
		}
	}

	// Exercise an actual executable path with spaces, without relying on 8.3 names.
	dir := t.TempDir()
	helper := filepath.Join(dir, "argument helper.exe")
	program, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(helper, program, 0700); err != nil {
		t.Fatal(err)
	}
	outputFile := filepath.Join(dir, "argument output.json")
	command := fmt.Sprintf(`"%s" -test.run=^TestWindowsArgumentHelper$ -- "two words" "a&b" >"%s"`, helper, outputFile)
	cmd := getCommand(context.Background(), command)
	cmd.Env = append(os.Environ(), "RUSH_WINDOWS_ARGUMENT_HELPER=1")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("quoted executable and redirection failed: %v: %s", err, output)
	}
	data, err := os.ReadFile(outputFile)
	if err != nil {
		t.Fatal(err)
	}
	var args []string
	if err := json.Unmarshal(data, &args); err != nil || len(args) != 2 || args[0] != "two words" || args[1] != "a&b" {
		t.Fatalf("arguments changed: %q (error=%v)", data, err)
	}
}

// TestWindowsCleanupHelperProcess keeps an owned process alive for cleanup tests.
func TestWindowsCleanupHelperProcess(t *testing.T) {
	if os.Getenv("RUSH_WINDOWS_CLEANUP_HELPER") != "1" {
		return
	}
	time.Sleep(30 * time.Second)
	os.Exit(0)
}

func TestWindowsForceCleanupContinuesAfterMemberFailure(t *testing.T) {
	for _, mode := range []string{"denied", "exited", "identity"} {
		t.Run(mode, func(t *testing.T) {
			// Both targets are owned helper processes. Visit the failing one first
			// so an early return cannot accidentally pass the cleanup assertion.
			type target struct {
				cmd    *exec.Cmd
				record windowsRecord
			}
			var targets []target
			for i := 0; i < 2; i++ {
				cmd := exec.Command(os.Args[0], "-test.run=^TestWindowsCleanupHelperProcess$")
				cmd.Env = append(os.Environ(), "RUSH_WINDOWS_CLEANUP_HELPER=1")
				if err := cmd.Start(); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
				})
				pid := uint32(cmd.Process.Pid)
				h, identity, err := openIdentifiedProcess(pid, windows.SYNCHRONIZE)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { windows.CloseHandle(h) })
				targets = append(targets, target{cmd, windowsRecord{pid: pid, handle: h, identity: identity, name: "cleanup-helper"}})
			}
			if targets[0].record.pid > targets[1].record.pid {
				targets[0], targets[1] = targets[1], targets[0]
			}
			root, failing := targets[0].record, targets[1]
			known := make(map[uint32]windowsProcess)
			for _, target := range targets {
				r := target.record
				known[r.pid] = windowsProcess{pid: r.pid, identity: r.identity, name: r.name}
			}
			originalOpen := openWindowsCleanupProcess
			t.Cleanup(func() { openWindowsCleanupProcess = originalOpen })
			attempts := 0
			openWindowsCleanupProcess = func(pid uint32, access uint32) (windows.Handle, uint64, error) {
				if pid != failing.record.pid {
					return originalOpen(pid, access)
				}
				attempts++
				switch mode {
				case "denied":
					return 0, 0, windows.ERROR_ACCESS_DENIED
				case "exited":
					if err := failing.cmd.Process.Kill(); err != nil {
						t.Fatal(err)
					}
					status, err := windows.WaitForSingleObject(failing.record.handle, 2000)
					if err != nil || status != windows.WAIT_OBJECT_0 {
						t.Fatalf("helper did not exit: status=%d, error=%v", status, err)
					}
					return 0, 0, windows.ERROR_INVALID_PARAMETER
				default:
					h, identity, err := originalOpen(pid, access)
					return h, identity + 1, err
				}
			}
			controller := &windowsController{opts: &Options{}}
			err := controller.forceTree(root, known)
			switch mode {
			case "denied":
				if !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
					t.Errorf("permission error lost: %v", err)
				}
			case "exited":
				if err != nil {
					t.Errorf("already-exited member caused cleanup failure: %v", err)
				}
			case "identity":
				if err == nil || !strings.Contains(err.Error(), "identity changed") {
					t.Errorf("identity error lost: %v", err)
				}
			}
			if attempts != 1 {
				t.Errorf("failing member visited %d times; want 1", attempts)
			}
			for _, target := range targets {
				alive, err := windowsProcessAlive(known[target.record.pid])
				wantAlive := target.record.pid == failing.record.pid && mode != "exited"
				if err != nil || alive != wantAlive {
					t.Errorf("pid %d: alive=%t, error=%v; want alive=%t", target.record.pid, alive, err, wantAlive)
				}
			}
		})
	}
}

// TestWindowsForceCleanupWaitsForExitingProcess exercises access denial while
// the process is still alive, followed by either exit or a genuine denial.
func TestWindowsForceCleanupWaitsForExitingProcess(t *testing.T) {
	for _, exits := range []bool{true, false} {
		t.Run(fmt.Sprintf("exits=%t", exits), func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestWindowsCleanupHelperProcess$")
			cmd.Env = append(os.Environ(), "RUSH_WINDOWS_CLEANUP_HELPER=1")
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
			pid := uint32(cmd.Process.Pid)
			h, identity, err := openIdentifiedProcess(pid, windows.SYNCHRONIZE)
			if err != nil {
				t.Fatal(err)
			}
			defer windows.CloseHandle(h)
			root := windowsRecord{pid: pid, handle: h, identity: identity, name: "cleanup-helper"}
			known := map[uint32]windowsProcess{pid: {pid: pid, identity: identity, name: root.name}}
			originalOpen := openWindowsCleanupProcess
			defer func() { openWindowsCleanupProcess = originalOpen }()
			var exited chan error
			openWindowsCleanupProcess = func(target uint32, access uint32) (windows.Handle, uint64, error) {
				if target != pid {
					return originalOpen(target, access)
				}
				// Omit terminate rights to obtain a real ERROR_ACCESS_DENIED.
				// Delay exit beyond the old zero-time wait, without mocking waits.
				if exits {
					exited = make(chan error, 1)
					go func() {
						time.Sleep(100 * time.Millisecond)
						exited <- cmd.Process.Kill()
					}()
				}
				return originalOpen(target, windows.SYNCHRONIZE)
			}
			controller := &windowsController{opts: &Options{}}
			err = controller.forceTree(root, known)
			if exits {
				if exited == nil {
					t.Fatal("cleanup did not attempt termination")
				}
				if killErr := <-exited; killErr != nil {
					t.Fatal(killErr)
				}
				if err != nil {
					t.Fatalf("exiting process caused cleanup failure: %v", err)
				}
			} else if !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
				t.Fatalf("permission error lost: %v", err)
			}
		})
	}
}

func TestWindowsExitedCommandCleanupPreservesRetries(t *testing.T) {
	opts := &Options{Jobs: 1, Retries: 1, PropExitStatus: true, CleanupTime: 0}
	got := observeRun(t, opts, "exit /b 7")
	if len(got.commands) != 1 || len(got.statuses) != 1 || got.statuses[0] != 7 || got.cause.Status != 0 {
		t.Fatalf("commands=%d, statuses=%v, cause=%v; want child exit status 7", len(got.commands), got.statuses, got.cause)
	}
}

func TestWindowsUnregisteredCleanupStopsDescendants(t *testing.T) {
	for _, exclude := range []bool{false, true} {
		cmd := getCommand(context.Background(), "ping -n 30 127.0.0.1 >NUL")
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		func() {
			defer func() {
				_ = terminateUnregisteredCommand(cmd, &Options{})
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
			}()
			deadline := time.Now().Add(3 * time.Second)
			var child windowsProcess
			for child.pid == 0 {
				processes, err := snapshotWindowsProcesses()
				if err != nil {
					t.Fatal(err)
				}
				for _, p := range processes {
					if p.ppid == uint32(cmd.Process.Pid) {
						child = p
						break
					}
				}
				if time.Now().After(deadline) {
					t.Fatal("child did not start")
				}
				if child.pid == 0 {
					time.Sleep(10 * time.Millisecond)
				}
			}
			opts := &Options{}
			if exclude {
				opts.NoKillExes = []string{child.name}
				opts.NoStopExes = []string{child.name}
			}
			if err := terminateUnregisteredCommand(cmd, opts); err != nil {
				t.Fatal(err)
			}
			alive, err := windowsProcessAlive(child)
			if err != nil || alive != exclude {
				t.Fatalf("excluded=%t: child alive=%t, err=%v", exclude, alive, err)
			}
		}()
	}
}

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
