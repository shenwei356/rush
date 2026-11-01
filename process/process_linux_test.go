//go:build linux

package process

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestLinuxProcessExitSnapshot(t *testing.T) {
	for _, state := range []string{"S", "Z", "X", "x"} {
		t.Run(state, func(t *testing.T) {
			group := "7"
			dead := state == "X" || state == "x"
			if dead {
				group = "-1" // A reaped task can lose its process-group mapping.
			}
			data := []byte("7 (worker (old)) " + state + " 1 " + group + strings.Repeat(" 0", 16) + " 99")
			p, err := parseLinuxProcessStat(7, data)
			if dead {
				if !unixProcessGone(err) {
					t.Fatalf("dead snapshot treated as an identity failure: process=%+v err=%v", p, err)
				}
			} else if err != nil || p.pid != 7 || p.pgid != 7 || p.identity != 99 || p.zombie != (state == "Z") {
				t.Fatalf("live/zombie identity was lost: process=%+v err=%v", p, err)
			}
		})
	}
}

func TestLinuxLongExecutableExclusions(t *testing.T) {
	sleepPath, err := exec.LookPath("sleep")
	if err != nil {
		t.Fatal(err)
	}
	program, err := os.ReadFile(sleepPath)
	if err != nil {
		t.Fatal(err)
	}
	const name = "rush-long-running-worker"
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, program, 0700); err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{"graceful", "force", "unrelated", "empty"} {
		t.Run(phase, func(t *testing.T) {
			opts := &Options{NoStopExes: []string{"all"}, NoKillExes: []string{name}}
			if phase == "graceful" {
				opts.NoStopExes, opts.NoKillExes = []string{name}, []string{"all"}
			} else if phase == "unrelated" {
				opts.NoKillExes = []string{"another-program"}
			} else if phase == "empty" {
				opts.NoStopExes, opts.NoKillExes = []string{"", ""}, []string{""}
			}
			controller, err := newPlatformProcessController(opts)
			if err != nil {
				t.Fatal(err)
			}
			defer controller.Close()
			cmd := exec.CommandContext(context.Background(), path, "30")
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			waited := make(chan struct{})
			go func() { _ = cmd.Wait(); close(waited) }()
			t.Cleanup(func() {
				_ = cmd.Process.Kill()
				<-waited
				controller.Finished(cmd)
			})
			if err := controller.Started(cmd); err != nil {
				t.Fatal(err)
			}
			p, err := lookupPlatformProcess(cmd.Process.Pid)
			if err != nil {
				t.Fatal(err)
			}
			got, err := fullPlatformProcessName(p)
			if err != nil || got != name {
				t.Errorf("full name=%q, err=%v; snapshot name=%q; want %q", got, err, p.name, name)
			}
			if phase == "empty" {
				// Empty exclusions must not require executable-name inspection.
				originalName := linuxProcessName
				t.Cleanup(func() { linuxProcessName = originalName })
				linuxProcessName = func(context.Context, int) (string, error) {
					return "", errors.New("unexpected executable-name lookup")
				}
			}
			if err := controller.KillCommand(cmd); err != nil {
				t.Fatal(err)
			}
			if phase == "unrelated" || phase == "empty" {
				select {
				case <-waited:
				case <-time.After(2 * time.Second):
					t.Error("unrelated exclusion prevented cleanup")
				}
				return
			}
			select {
			case <-waited:
				t.Error("excluded executable was terminated")
			case <-time.After(100 * time.Millisecond):
			}
		})
	}
}

func TestLinuxFullNameRejectsChangedIdentity(t *testing.T) {
	p, err := lookupPlatformProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	p.name = strings.Repeat("n", 15)
	p.identity++
	originalName := linuxProcessName
	t.Cleanup(func() { linuxProcessName = originalName })
	linuxProcessName = func(context.Context, int) (string, error) {
		return "/usr/bin/rush-long-running-worker", nil
	}
	if _, err := fullPlatformProcessName(p); err == nil || !strings.Contains(err.Error(), "identity changed") {
		t.Fatalf("stale process identity accepted: %v", err)
	}
}

func TestSignalSelectionHonorsExclusions(t *testing.T) {
	tests := []struct {
		name           string
		noStop, noKill []string
		want           int
	}{
		{"both", nil, nil, SEND_CTRL_C_SIGNAL | SEND_CTRL_BREAK_SIGNAL | SEND_KILL_SIGNAL},
		{"no stop", []string{"worker"}, nil, SEND_KILL_SIGNAL},
		{"no kill", nil, []string{"worker"}, SEND_CTRL_C_SIGNAL | SEND_CTRL_BREAK_SIGNAL},
		{"none", []string{"all"}, []string{"all"}, SEND_NO_SIGNAL},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := getSignalsToSend("worker", tt.noStop, tt.noKill)
			if err != nil || got != tt.want {
				t.Fatalf("got %d, %v; want %d", got, err, tt.want)
			}
		})
	}
}

func TestIntSetAddIsAtomic(t *testing.T) {
	var set IntSet
	winners := make(chan bool, 32)
	for i := 0; i < cap(winners); i++ {
		go func() { winners <- set.Add(os.Getpid()) }()
	}
	n := 0
	for i := 0; i < cap(winners); i++ {
		if <-winners {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("winners = %d; want 1", n)
	}
}

func TestForceStopIsIdempotent(t *testing.T) {
	opts := &Options{}
	opts.ForceStop()
	opts.ForceStop()
	select {
	case <-opts.forceStopChannel():
	default:
		t.Fatal("force channel is open")
	}
}
