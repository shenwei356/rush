package process

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shenwei356/go-logging"
	"github.com/shenwei356/rush/internal/runstate"
)

func TestDryRunDoesNotProbeResources(t *testing.T) {
	for _, cancelPreview := range []bool{false, true} {
		name := "preview"
		if cancelPreview {
			name = "cancelled preview"
		}
		t.Run(name, func(t *testing.T) {
			oldMemory, oldLoad, oldPoll := availableMemory, systemLoad, resourcePollInterval
			t.Cleanup(func() { availableMemory, systemLoad, resourcePollInterval = oldMemory, oldLoad, oldPoll })
			resourcePollInterval = 5 * time.Millisecond
			var calls atomic.Int32
			var once sync.Once
			probed := make(chan struct{})
			probe := func() error {
				calls.Add(1)
				once.Do(func() { close(probed) })
				return errors.New("resource probe unavailable")
			}
			availableMemory = func() (uint64, uint64, error) { return 0, 0, probe() }
			systemLoad = func() (float64, error) { return 0, probe() }
			parent, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			state, ctx := runstate.New(parent)
			defer state.Cancel()
			input := make(chan string)
			opts := &Options{Jobs: 1, DryRun: true, StartDelay: time.Hour, MaxLoad: 1, MinFreeMemory: 1, PropExitStatus: true, RecordSuccessfulCmd: true}
			out, success, done, statuses := Run4OutputContext(opts, ctx, state.Cancel, input)
			t.Cleanup(func() {
				state.Cancel()
				<-done // Join the scheduler before restoring the probe hooks.
			})
			// Keep input pending across several monitoring intervals, as with
			// slow stdin, so a background probe cannot escape the regression.
			select {
			case <-probed:
				t.Error("dry run queried execution resources while waiting for input")
			case <-time.After(10 * resourcePollInterval):
			}
			wantCause := runstate.Cause{}
			if cancelPreview {
				wantCause = runstate.Cause{Kind: runstate.Signal, Status: 130}
				state.Stop(wantCause)
			} else {
				select {
				case input <- "echo preview":
				case <-ctx.Done():
				}
			}
			close(input)
			var output strings.Builder
			for part := range out {
				output.WriteString(part)
			}
			<-done
			wantOutput, wantStatuses := "echo preview\n", 1
			if cancelPreview {
				wantOutput, wantStatuses = "", 0
			}
			if got := output.String(); got != wantOutput {
				t.Errorf("output=%q; want %q", got, wantOutput)
			}
			count := 0
			for status := range statuses {
				count++
				if status != 0 {
					t.Errorf("preview status=%d; want 0", status)
				}
			}
			if count != wantStatuses || calls.Load() != 0 || state.Cause() != wantCause {
				t.Errorf("statuses=%d probes=%d cause=%+v; want %d, 0, %+v", count, calls.Load(), state.Cause(), wantStatuses, wantCause)
			}
			if record, ok := <-success; ok {
				t.Errorf("preview recorded a successful execution: %q", record)
			}
		})
	}
}

// blockedResourceLog models a diagnostic destination that is no longer draining.
type blockedResourceLog struct {
	entered chan struct{} // Closed when diagnostic delivery starts.
	release chan struct{} // Closed after checking cancellation.
	once    sync.Once     // Notify entry only once, even if more logs arrive.
}

// Write holds diagnostic delivery until the test releases it.
func (w *blockedResourceLog) Write(data []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	<-w.release
	return len(data), nil
}

func TestMemoryProbeFailureCancelsBeforeLogging(t *testing.T) {
	oldMemory, oldPoll, oldLog := availableMemory, resourcePollInterval, Log
	logs := &blockedResourceLog{entered: make(chan struct{}), release: make(chan struct{})}
	availableMemory = func() (uint64, uint64, error) { return 0, 0, errors.New("probe failed") }
	resourcePollInterval = 5 * time.Millisecond
	Log = logging.MustGetLogger("blocked-resource-log")
	Log.SetBackend(logging.AddModuleLevel(logging.NewLogBackend(logs, "", 0)))
	state, ctx := runstate.New(context.Background())
	gate := newStartGate(ctx, &Options{MinFreeMemory: 1}, state)
	defer func() {
		close(logs.release)
		gate.close()
		availableMemory, resourcePollInterval, Log = oldMemory, oldPoll, oldLog
	}()
	select {
	case <-logs.entered:
	case <-time.After(time.Second):
		t.Fatal("monitor did not report probe failure")
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Error("blocked diagnostic delayed cancellation")
	}
	if cause := state.Cause(); cause != (runstate.Cause{Kind: runstate.Internal, Status: 1}) {
		t.Errorf("cause=%+v; want internal failure", cause)
	}
}
