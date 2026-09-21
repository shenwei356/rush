//go:build !windows

package process

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shenwei356/rush/internal/runstate"
)

type recordingController struct {
	processController
	started chan time.Time
	killed  chan string
}

func (c *recordingController) Started(cmd *exec.Cmd) error {
	c.started <- time.Now()
	return c.processController.Started(cmd)
}
func (c *recordingController) KillCommand(cmd *exec.Cmd) error {
	err := c.processController.KillCommand(cmd)
	if c.killed != nil {
		c.killed <- strings.Join(cmd.Args, " ")
	}
	return err
}

func recordStarts(t *testing.T, killed bool) *recordingController {
	t.Helper()
	old := makeProcessController
	recorded := &recordingController{started: make(chan time.Time, 16)}
	if killed {
		recorded.killed = make(chan string, 4)
	}
	makeProcessController = func(opts *Options) (processController, error) {
		inner, err := newPlatformProcessController(opts)
		recorded.processController = inner
		return recorded, err
	}
	t.Cleanup(func() { makeProcessController = old })
	return recorded
}

func TestStartDelaySeparatesActualStarts(t *testing.T) {
	recorded := recordStarts(t, false)
	delay := 80 * time.Millisecond
	opts := &Options{Jobs: 3, StartDelay: delay, PropExitStatus: true}
	got := observeRun(t, opts, "true", "true", "true")
	if len(got.commands) != 3 {
		t.Fatalf("completed %d commands; want 3", len(got.commands))
	}
	var previous time.Time
	for i := 0; i < 3; i++ {
		started := <-recorded.started
		if i > 0 && started.Sub(previous) < delay-10*time.Millisecond {
			t.Fatal("command starts were closer than --delay")
		}
		previous = started
	}
}

func TestLoadWaitAndCancellation(t *testing.T) {
	recorded := recordStarts(t, false)
	oldLoad, oldPoll := systemLoad, resourcePollInterval
	var current atomic.Int64
	current.Store(10)
	systemLoad = func() (float64, error) { return float64(current.Load()), nil }
	resourcePollInterval = 20 * time.Millisecond
	t.Cleanup(func() { systemLoad, resourcePollInterval = oldLoad, oldPoll })

	ctx, cancel := context.WithCancel(context.Background())
	opts := &Options{Jobs: 1, MaxLoad: 1}
	input := make(chan string, 1)
	input <- "true"
	close(input)
	commands, _, done, _ := runContext(opts, ctx, cancel, input)
	commandDone := make(chan struct{})
	go func() {
		for command := range commands {
			for range command.Ch {
			}
			_ = command.Cleanup()
		}
		close(commandDone)
	}()
	select {
	case <-recorded.started:
		t.Fatal("job started above --load threshold")
	case <-time.After(80 * time.Millisecond):
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancellation did not interrupt resource wait")
	}
	<-commandDone
	if len(recorded.started) != 0 {
		t.Fatal("job started after cancellation")
	}

	current.Store(0)
	got := observeRun(t, &Options{Jobs: 1, MaxLoad: 1, PropExitStatus: true}, "true")
	if len(got.commands) != 1 || len(recorded.started) != 1 {
		t.Fatalf("after load recovered: commands=%d starts=%d", len(got.commands), len(recorded.started))
	}
}

func TestMemoryPressureRequeuesWithoutConsumingRetries(t *testing.T) {
	recorded := recordStarts(t, true)
	oldMemory, oldPoll := availableMemory, resourcePollInterval
	var free atomic.Uint64
	free.Store(200)
	availableMemory = func() (uint64, uint64, error) { return free.Load(), 400, nil }
	resourcePollInterval = 20 * time.Millisecond
	t.Cleanup(func() { availableMemory, resourcePollInterval = oldMemory, oldPoll })

	marker := t.TempDir() + "/started"
	cmd := fmt.Sprintf("if [ -e '%s' ]; then printf recovered; else touch '%s'; printf partial; sleep 30; fi", marker, marker)
	state, ctx := runstate.New(context.Background())
	defer state.Cancel()
	input := make(chan string, 1)
	input <- cmd
	close(input)
	opts := &Options{Jobs: 1, MinFreeMemory: 100, CleanupTime: 0, StopOnErr: true, PropExitStatus: true, RecordSuccessfulCmd: true}
	out, success, done, statuses := Run4OutputContext(opts, ctx, state.Cancel, input)
	output := make(chan string, 1)
	go func() {
		var text strings.Builder
		for part := range out {
			text.WriteString(part)
		}
		output <- text.String()
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first attempt did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	free.Store(40)
	select {
	case <-recorded.killed:
	case <-time.After(3 * time.Second):
		t.Fatal("low memory did not stop the youngest job")
	}
	free.Store(200)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("requeued job did not complete")
	}
	if got := <-output; got != "recovered" {
		t.Fatalf("output=%q; want only successful attempt", got)
	}
	if got := <-statuses; got != 0 {
		t.Fatalf("status=%d; want 0", got)
	}
	if got := <-success; got != cmd {
		t.Fatalf("success record=%q; want %q", got, cmd)
	}
	if len(recorded.started) != 2 {
		t.Fatalf("started %d attempts; want 2", len(recorded.started))
	}
	if cause := state.Cause(); cause != (runstate.Cause{}) {
		t.Fatalf("memory stop set terminal cause: %+v", cause)
	}
}

func TestCancellationAfterMemoryRequeue(t *testing.T) {
	recorded := recordStarts(t, true)
	oldMemory, oldPoll := availableMemory, resourcePollInterval
	var free atomic.Uint64
	free.Store(200)
	availableMemory = func() (uint64, uint64, error) { return free.Load(), 400, nil }
	resourcePollInterval = 20 * time.Millisecond
	t.Cleanup(func() { availableMemory, resourcePollInterval = oldMemory, oldPoll })

	marker := t.TempDir() + "/started"
	state, ctx := runstate.New(context.Background())
	defer state.Cancel()
	input := make(chan string, 1)
	input <- fmt.Sprintf("touch '%s'; sleep 30", marker)
	close(input)
	opts := &Options{Jobs: 1, MinFreeMemory: 100, CleanupTime: 0, PropExitStatus: true}
	out, _, done, _ := Run4OutputContext(opts, ctx, state.Cancel, input)
	go func() {
		for range out {
		}
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first attempt did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	free.Store(40)
	select {
	case <-recorded.killed:
	case <-time.After(3 * time.Second):
		t.Fatal("low memory did not stop the job")
	}
	state.Cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancellation did not stop the queued retry")
	}
}

func TestMemoryPressureStopsYoungestAndKeepsOutputOrder(t *testing.T) {
	recorded := recordStarts(t, true)
	oldMemory, oldPoll := availableMemory, resourcePollInterval
	var free atomic.Uint64
	free.Store(200)
	availableMemory = func() (uint64, uint64, error) { return free.Load(), 400, nil }
	resourcePollInterval = 20 * time.Millisecond
	t.Cleanup(func() { availableMemory, resourcePollInterval = oldMemory, oldPoll })

	marker := t.TempDir() + "/youngest"
	state, ctx := runstate.New(context.Background())
	defer state.Cancel()
	input := make(chan string)
	sendYoungest := make(chan struct{})
	go func() {
		input <- "sleep 1; printf older"
		<-sendYoungest
		input <- fmt.Sprintf("if [ -e '%s' ]; then printf younger; else touch '%s'; printf partial; sleep 30; fi", marker, marker)
		close(input)
	}()
	opts := &Options{Jobs: 2, KeepOrder: true, MinFreeMemory: 100, CleanupTime: 0, PropExitStatus: true}
	out, _, done, statuses := Run4OutputContext(opts, ctx, state.Cancel, input)
	select {
	case <-recorded.started:
		close(sendYoungest)
	case <-time.After(3 * time.Second):
		t.Fatal("older job did not start")
	}
	output := make(chan string, 1)
	go func() {
		var text strings.Builder
		for part := range out {
			text.WriteString(part)
		}
		output <- text.String()
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("youngest job did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	free.Store(40)
	select {
	case killed := <-recorded.killed:
		if !strings.Contains(killed, marker) {
			t.Fatalf("killed %q; want youngest command", killed)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("low memory did not stop a job")
	}
	free.Store(200)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("jobs did not complete")
	}
	if got := <-output; got != "olderyounger" {
		t.Fatalf("output=%q; want ordered final output", got)
	}
	var gotStatuses []int
	for status := range statuses {
		gotStatuses = append(gotStatuses, status)
	}
	if len(gotStatuses) != 2 || gotStatuses[0] != 0 || gotStatuses[1] != 0 || len(recorded.started) != 2 {
		t.Fatalf("statuses=%v, later starts=%d; want two successes and three starts", gotStatuses, len(recorded.started))
	}
}

func TestResourceProbeFailuresStopRun(t *testing.T) {
	oldLoad, oldMemory := systemLoad, availableMemory
	t.Cleanup(func() { systemLoad, availableMemory = oldLoad, oldMemory })
	for _, tt := range []struct {
		name  string
		opts  *Options
		setup func()
	}{
		{"load read error", &Options{Jobs: 1, MaxLoad: 1, PropExitStatus: true}, func() {
			systemLoad = func() (float64, error) { return 0, errors.New("load unavailable") }
		}},
		{"memory exceeds total", &Options{Jobs: 1, MinFreeMemory: 500, PropExitStatus: true}, func() {
			availableMemory = func() (uint64, uint64, error) { return 100, 400, nil }
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tt.setup()
			got := observeRun(t, tt.opts, "true")
			if got.cause.Kind != runstate.Internal || len(got.statuses) != 1 || got.statuses[0] != 1 {
				t.Fatalf("cause=%+v, statuses=%v; want internal failure and status 1", got.cause, got.statuses)
			}
			if len(got.commands) != 1 || got.commands[0].Err == nil {
				t.Fatalf("commands=%d; want a failed command", len(got.commands))
			}
		})
	}
}
