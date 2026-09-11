//go:build linux

package process

import (
	"os"
	"os/exec"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestProcessInspectionDoesNotNeedExternalCommands(t *testing.T) {
	if os.Getenv("RUSH_PROCESS_TEST_HELPER") == "1" {
		time.Sleep(30 * time.Second)
		return
	}

	oldMarker := ChildMarker
	oldPidRecords := pidRecords
	ChildMarker = "test_" + strconv.Itoa(os.Getpid())
	pidRecords = make(map[int]ProcessRecord)
	t.Cleanup(func() {
		ChildMarker = oldMarker
		pidRecords = oldPidRecords
	})

	cmd := exec.Command(os.Args[0], "-test.run=^TestProcessInspectionDoesNotNeedExternalCommands$")
	cmd.Env = append(os.Environ(),
		"RUSH_PROCESS_TEST_HELPER=1",
		getChildMarkerKey()+"="+getChildMarkerValue(),
	)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
	})

	t.Setenv("PATH", "")
	records := getChildProcesses(&sync.Mutex{}, nil, nil)
	for _, record := range records {
		if record.pid == cmd.Process.Pid {
			return
		}
	}
	t.Fatalf("marked child process %d was not found without ps, kill, sh, or xargs in PATH", cmd.Process.Pid)
}

func TestReleaseProcessesClearsRecords(t *testing.T) {
	oldPidRecords := pidRecords
	pidRecords = map[int]ProcessRecord{
		12345: {pid: 12345, processHandle: 12345},
	}
	t.Cleanup(func() {
		pidRecords = oldPidRecords
	})

	releaseProcesses(&sync.Mutex{})
	if len(pidRecords) != 0 {
		t.Fatalf("process records were not released: %v", pidRecords)
	}
}

func TestSignalSelectionHonorsExclusions(t *testing.T) {
	tests := []struct {
		name       string
		noStopExes []string
		noKillExes []string
		want       int
	}{
		{name: "stop and kill", want: SEND_CTRL_C_SIGNAL | SEND_KILL_SIGNAL},
		{name: "skip graceful stop by name", noStopExes: []string{"worker"}, want: SEND_KILL_SIGNAL},
		{name: "skip graceful stop for all", noStopExes: []string{"all"}, want: SEND_KILL_SIGNAL},
		{name: "skip force kill by name", noKillExes: []string{"worker"}, want: SEND_CTRL_C_SIGNAL},
		{name: "skip every signal", noStopExes: []string{"all"}, noKillExes: []string{"all"}, want: SEND_NO_SIGNAL},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := getSignalsToSend("worker", tt.noStopExes, tt.noKillExes)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("signals: %d; want %d", got, tt.want)
			}
		})
	}
}

func TestContainsMarkerMatchesExactAndNestedValues(t *testing.T) {
	oldMarker := ChildMarker
	ChildMarker = "123_ab"
	t.Cleanup(func() {
		ChildMarker = oldMarker
	})

	tests := []struct {
		name string
		env  string
		want bool
	}{
		{name: "exact", env: "A=1\x00RUSH_CHILD_GROUP=[123_ab]\x00", want: true},
		{name: "nested", env: "RUSH_CHILD_GROUP=[parent_1][123_ab]", want: true},
		{name: "different marker", env: "RUSH_CHILD_GROUP=[123_abc]", want: false},
		{name: "different variable", env: "OTHER=[123_ab]", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := containsMarker(tt.env); got != tt.want {
				t.Fatalf("containsMarker(%q): %t; want %t", tt.env, got, tt.want)
			}
		})
	}
}

func TestForceStopIsIdempotent(t *testing.T) {
	opts := &Options{}
	opts.ForceStop()
	opts.ForceStop()
	select {
	case <-opts.forceStopChannel():
	default:
		t.Fatal("force-stop channel is not closed")
	}
}
