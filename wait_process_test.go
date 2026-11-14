package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func TestWaitForProcessStableCondition(t *testing.T) {
	for _, wantPresent := range []bool{true, false} {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		condition := processWait{name: "worker", present: wantPresent, duration: 40 * time.Millisecond, interval: 5 * time.Millisecond}
		calls := 0
		var restarted time.Time
		err := waitForProcess(ctx, condition, func(context.Context) (bool, error) {
			calls++
			// A brief reversal must restart the entire stability period.
			if calls == 2 {
				return !wantPresent, nil
			}
			if calls == 3 {
				restarted = time.Now()
			}
			return wantPresent, nil
		})
		cancel()
		if err != nil || restarted.IsZero() || time.Since(restarted) < condition.duration {
			t.Fatalf("present=%t: condition released early: calls=%d err=%v", wantPresent, calls, err)
		}
	}
}

func TestWaitForProcessCancellationAndErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	err := waitForProcess(ctx, processWait{present: true, interval: time.Hour}, func(context.Context) (bool, error) {
		cancel()
		return false, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	failure := errors.New("permission denied")
	err = waitForProcess(context.Background(), processWait{present: false, interval: time.Millisecond}, func(context.Context) (bool, error) {
		return false, failure
	})
	if !errors.Is(err, failure) {
		t.Fatalf("lookup failure treated as absence: %v", err)
	}
}

func TestWaitForProcessInterval(t *testing.T) {
	condition, err := parseProcessWait("worker", "", 0, 0.025, false, true, false)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 750*time.Millisecond)
	defer cancel()
	var firstScanDone time.Time
	calls := 0
	err = waitForProcess(ctx, condition, func(context.Context) (bool, error) {
		calls++
		if calls == 1 {
			// The interval starts after a scan completes, avoiding overlapping scans.
			time.Sleep(10 * time.Millisecond)
			firstScanDone = time.Now()
			return false, nil
		}
		if time.Since(firstScanDone) < 25*time.Millisecond {
			t.Error("next scan started before the configured interval elapsed")
		}
		return true, nil
	})
	if err != nil || calls != 2 {
		t.Fatalf("custom interval was not used: calls=%d, err=%v", calls, err)
	}
}

func TestProcessWaitInvalidFlagsPreserveOutput(t *testing.T) {
	for _, flags := range [][]string{
		{"--wait-for-process", "a", "--wait-for-process-exit", "b"},
		{"--wait-for-process", ""},
		{"--wait-for-process-exit", ""},
		{"--wait-for-process", "/bin/worker"},
		{"-U", "0"},
		{"--wait-for-process", "worker", "-U", "-1"},
		{"--wait-for-process", "worker", "--wait-for-process-duration", "NaN"},
		{"--wait-for-process", "worker", "--wait-for-process-duration", "+Inf"},
		{"--wait-for-process", "worker", "--wait-for-process-duration", "1e20"},
		{"--wait-for-process-regexp"},
		{"-W", "[", "-R"},
		{"-w", "[", "--wait-for-process-regexp", "--dry-run"},
		{"-W", "worker", "-w", "helper"},
		{"-u", "1"},
		{"-W", "worker", "-u", "0"},
		{"-W", "worker", "-u", "-1"},
		{"-W", "worker", "-u", "NaN"},
		{"-W", "worker", "-u", "+Inf"},
		{"-W", "worker", "-u", "1e20"},
		{"-W", "worker", "--wait-for-process-interval", "1e-20", "--dry-run"},
	} {
		t.Run(strings.Join(flags, " "), func(t *testing.T) {
			output := t.TempDir() + "/output"
			if err := os.WriteFile(output, []byte("preserved"), 0600); err != nil {
				t.Fatal(err)
			}
			journal := t.TempDir() + "/journal"
			args := append([]string{"-o", output, "-c", "-C", journal}, flags...)
			stdout, stderr, code := runLegacyRush(t, "x\n", nil, append(args, "echo executed")...)
			if code == 0 || stdout != "" || !strings.Contains(stderr, "process") {
				t.Fatalf("invalid flags: exit=%d stdout=%q stderr=%q", code, stdout, stderr)
			}
			if data, err := os.ReadFile(output); err != nil || string(data) != "preserved" {
				t.Fatalf("output changed: %q, %v", data, err)
			}
			if _, err := os.Stat(journal); !os.IsNotExist(err) {
				t.Fatalf("invalid flags created a journal: %v", err)
			}
		})
	}
}

func TestProcessWaitDryRun(t *testing.T) {
	stdout, stderr, code := runLegacyRush(t, "x\n", nil, "--dry-run", "--wait-for-process", "rush-nonexistent-test-worker", "--wait-for-process-duration", "3600", "echo {}")
	if code != 0 || normalizedLines(stdout) != "echo\nx" || stderr != "" {
		t.Fatalf("dry run: exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}
