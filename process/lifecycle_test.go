package process

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/schollz/progressbar/v3"
	"github.com/shenwei356/go-logging"
	"github.com/shenwei356/rush/internal/runstate"
)

// trackedParent observes cancellation subscriptions without inspecting the
// standard library's private context implementation.
type trackedParent struct {
	context.Context
	active atomic.Int64
}

func (p *trackedParent) Value(any) any { return nil } // Expose the AfterFunc path.
func (p *trackedParent) AfterFunc(f func()) func() bool {
	p.active.Add(1)
	stop := context.AfterFunc(p.Context, f)
	return func() bool {
		if stop() {
			p.active.Add(-1)
			return true
		}
		return false
	}
}

func TestContextRunReleasesOwnedContext(t *testing.T) {
	for _, mode := range []string{"owned", "owned-no-stop", "borrowed"} {
		t.Run(mode, func(t *testing.T) {
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			tracked := &trackedParent{Context: parent}
			var ctx context.Context = tracked
			var borrowed *runstate.State
			wantActive := int64(0)
			if mode == "borrowed" {
				borrowed, ctx = runstate.New(tracked)
				defer borrowed.Cancel()
				wantActive = 1
			}
			var stop context.CancelFunc = cancel
			if mode == "owned-no-stop" {
				stop = nil
			}
			input := make(chan string, 1)
			input <- "echo output"
			close(input)
			out, success, done, _ := Run4OutputContext(&Options{Jobs: 1}, ctx, stop, input)
			for range out {
			}
			for range success {
			}
			<-done
			if parent.Err() != nil || mode == "borrowed" && ctx.Err() != nil {
				t.Fatal("successful run cancelled its caller")
			}
			if active := tracked.active.Load(); active != wantActive {
				t.Fatalf("completed run retained %d parent cancellation subscriptions; want %d", active, wantActive)
			}
			if borrowed != nil {
				borrowed.Cancel()
				if parent.Err() != nil {
					t.Fatal("completed run retained its cancellation callback")
				}
			}
		})
	}
}

func TestContextRunFailureStopsCallerProducer(t *testing.T) {
	for _, setupFailure := range []bool{false, true} {
		for _, sharedState := range []bool{false, true} {
			t.Run(fmt.Sprintf("setup=%t/shared=%t", setupFailure, sharedState), func(t *testing.T) {
				if setupFailure {
					original := makeProcessController
					makeProcessController = func(*Options) (processController, error) {
						return nil, errors.New("controller setup failed")
					}
					t.Cleanup(func() { makeProcessController = original })
				}
				ctx, cancel := context.WithCancel(context.Background())
				runCtx := ctx
				if sharedState {
					var state *runstate.State
					state, runCtx = runstate.New(ctx)
					defer state.Cancel()
				}
				input := make(chan string)
				producerDone := make(chan struct{})
				go func() {
					defer close(producerDone)
					defer close(input)
					for {
						select {
						case input <- "exit 7":
						case <-ctx.Done():
							return
						}
					}
				}()
				t.Cleanup(func() { cancel(); <-producerDone })
				out, success, done, _ := Run4OutputContext(&Options{Jobs: 1, StopOnErr: true}, runCtx, cancel, input)
				for range out {
				}
				for range success {
				}
				<-done
				if ctx.Err() != context.Canceled {
					t.Error("run failure did not cancel its caller")
				}
				select {
				case <-producerDone:
				case <-time.After(time.Second):
					t.Error("caller producer remained blocked after the failed run")
				}
			})
		}
	}
}

func TestConcurrentRunsKeepVerbosityLocal(t *testing.T) {
	oldLog, oldVerbose := Log, Verbose
	t.Cleanup(func() { Log, Verbose = oldLog, oldVerbose })
	var logs bytes.Buffer
	Log = logging.MustGetLogger("verbosity-isolation")
	Log.SetBackend(logging.AddModuleLevel(logging.NewLogBackend(&logs, "", 0)))
	Verbose = true // Legacy standalone defaults must not override run options.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ready := make(chan struct{}, 2)
	results := make(chan error, 2)
	inputs := []chan string{make(chan string, 1), make(chan string, 1)}
	for i, input := range inputs {
		go func(i int, input chan string) {
			opts := &Options{Jobs: 1, Verbose: i == 0, PropExitStatus: true, OutFileHandle: os.Stdout, ErrFileHandle: os.Stderr}
			out, success, done, statuses := Run4OutputContext(opts, ctx, cancel, input)
			ready <- struct{}{}
			var output strings.Builder
			for s := range out {
				output.WriteString(s)
			}
			for range success {
			}
			status, count := 0, 0
			for code := range statuses {
				status, count = code, count+1
			}
			<-done
			want := []string{"verbose-run", "quiet-run"}[i]
			if status != 0 || count != 1 || strings.TrimSpace(output.String()) != want {
				results <- fmt.Errorf("run %d: status=%d count=%d output=%q", i, status, count, output.String())
				return
			}
			results <- nil
		}(i, input)
	}
	// Start commands only after both schedulers have applied their options.
	<-ready
	<-ready
	for i, text := range []string{"echo verbose-run", "echo quiet-run"} {
		inputs[i] <- text
		close(inputs[i])
	}
	for range inputs {
		if err := <-results; err != nil {
			t.Error(err)
		}
	}
	text := logs.String()
	if !strings.Contains(text, "start cmd #1: echo verbose-run") || !strings.Contains(text, "finish cmd #1: echo verbose-run") || strings.Contains(text, "quiet-run") {
		t.Errorf("run verbosity was not isolated: %q", text)
	}
	if !Verbose {
		t.Error("scheduler changed the standalone verbosity default")
	}
}

func TestStandaloneCommandVerbosity(t *testing.T) {
	oldLog, oldVerbose := Log, Verbose
	t.Cleanup(func() { Log, Verbose = oldLog, oldVerbose })
	for _, legacy := range []bool{false, true} {
		for _, option := range []bool{false, true} {
			t.Run(fmt.Sprintf("legacy=%t/option=%t", legacy, option), func(t *testing.T) {
				var logs bytes.Buffer
				Log = logging.MustGetLogger("standalone-verbosity")
				Log.SetBackend(logging.AddModuleLevel(logging.NewLogBackend(&logs, "", 0)))
				Verbose = legacy
				command := NewCommand(1, "echo standalone", nil, 0)
				out, err := command.Run(&Options{Verbose: option, OutFileHandle: os.Stdout, ErrFileHandle: os.Stderr}, 1)
				for range out {
				}
				outputErr := <-command.outputDone
				cleanupErr := command.Cleanup()
				if err != nil || outputErr != nil || cleanupErr != nil {
					t.Fatalf("Run=%v output=%v cleanup=%v", err, outputErr, cleanupErr)
				}
				if logged := strings.Contains(logs.String(), "start cmd #1: echo standalone"); logged != (legacy || option) {
					t.Fatalf("unexpected verbosity: %q", logs.String())
				}
			})
		}
	}
}

func withSpillSeams(t *testing.T) {
	t.Helper()
	oldCreate, oldFlush, oldSeek, oldClose, oldRemove := createSpillFile, flushSpillFile, seekSpillFile, closeSpillFile, removeSpillFile
	oldWrite, oldReader := writeSpillFile, wrapSpillReader
	t.Cleanup(func() {
		createSpillFile = oldCreate
		flushSpillFile = oldFlush
		seekSpillFile = oldSeek
		closeSpillFile = oldClose
		removeSpillFile = oldRemove
		writeSpillFile = oldWrite
		wrapSpillReader = oldReader
	})
}

func TestCommandSetupFailureCompletesNotifications(t *testing.T) {
	for _, dryrun := range []bool{false, true} {
		old := makeProcessController
		sentinel := errors.New("controller setup failed")
		makeProcessController = func(*Options) (processController, error) { return nil, sentinel }
		cancel := make(chan struct{})
		if dryrun {
			close(cancel)
			sentinel = ErrCancelled
		}
		command := NewCommand(1, "unused", cancel, 0)
		command.dryrun = dryrun
		out, err := command.Run(&Options{}, 1)
		makeProcessController = old
		if !errors.Is(err, sentinel) || !errors.Is(command.Err, sentinel) || command.exitStatus == 0 {
			t.Fatalf("Run=%v, command error=%v, status=%d", err, command.Err, command.exitStatus)
		}
		select {
		case _, open := <-out:
			if open {
				t.Fatal("setup failure produced output")
			}
		default:
			t.Fatal("output channel remains open")
		}
		select {
		case _, open := <-command.Executed:
			if open {
				t.Fatal("setup failure was marked executed")
			}
		default:
			t.Fatal("execution notification remains open")
		}
		select {
		case err := <-command.outputDone:
			if err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatal("output completion was not delivered")
		}
	}
}

func TestSpillWriterPropagatesLifecycleFailures(t *testing.T) {
	sentinel := errors.New("fault")
	t.Run("create", func(t *testing.T) {
		withSpillSeams(t)
		createSpillFile = func() (*os.File, error) { return nil, sentinel }
		w := newSpillWriter(0)
		if _, err := w.Write([]byte("x")); !errors.Is(err, sentinel) {
			t.Fatalf("error=%v", err)
		}
	})
	t.Run("flush", func(t *testing.T) {
		withSpillSeams(t)
		flushSpillFile = func(*bufio.Writer) error { return sentinel }
		w := newSpillWriter(0)
		_, _ = w.Write([]byte("x"))
		if _, err := w.finish(); !errors.Is(err, sentinel) {
			t.Fatalf("error=%v", err)
		}
		c := &Command{tmpfh: w.file, tmpfile: w.name}
		_ = c.Cleanup()
	})
	t.Run("write", func(t *testing.T) {
		withSpillSeams(t)
		writeSpillFile = func(*bufio.Writer, []byte) (int, error) { return 0, sentinel }
		w := newSpillWriter(0)
		if _, err := w.Write([]byte("x")); !errors.Is(err, sentinel) {
			t.Fatalf("error=%v", err)
		}
		c := &Command{tmpfh: w.file, tmpfile: w.name}
		_ = c.Cleanup()
	})
	t.Run("seek", func(t *testing.T) {
		withSpillSeams(t)
		seekSpillFile = func(*os.File, int64, int) (int64, error) { return 0, sentinel }
		w := newSpillWriter(0)
		_, _ = w.Write([]byte("x"))
		if _, err := w.finish(); !errors.Is(err, sentinel) {
			t.Fatalf("error=%v", err)
		}
		c := &Command{tmpfh: w.file, tmpfile: w.name}
		_ = c.Cleanup()
	})
	t.Run("close", func(t *testing.T) {
		withSpillSeams(t)
		f, err := os.CreateTemp(t.TempDir(), "spill")
		if err != nil {
			t.Fatal(err)
		}
		closeSpillFile = func(*os.File) error { return sentinel }
		c := &Command{tmpfh: f}
		if err := c.Cleanup(); !errors.Is(err, sentinel) {
			t.Fatalf("error=%v", err)
		}
		_ = f.Close()
	})
	t.Run("remove", func(t *testing.T) {
		withSpillSeams(t)
		f, err := os.CreateTemp(t.TempDir(), "spill")
		if err != nil {
			t.Fatal(err)
		}
		name := f.Name()
		_ = f.Close()
		removeSpillFile = func(string) error { return sentinel }
		c := &Command{tmpfile: name}
		if err := c.Cleanup(); !errors.Is(err, sentinel) {
			t.Fatalf("error=%v", err)
		}
		_ = os.Remove(name)
	})
	t.Run("read", func(t *testing.T) {
		withSpillSeams(t)
		wrapSpillReader = func(io.Reader) io.Reader { return errorReader{err: sentinel} }
		w := newSpillWriter(1)
		_, _ = w.Write([]byte("x"))
		r, err := w.finish()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.Read(make([]byte, 1)); !errors.Is(err, sentinel) {
			t.Fatalf("error=%v", err)
		}
	})
}

func TestCommandOutputErrorPreservesCause(t *testing.T) {
	withSpillSeams(t)
	oldLimit := TmpOutputDataBuffer
	TmpOutputDataBuffer = 0
	t.Cleanup(func() { TmpOutputDataBuffer = oldLimit })
	// Flush happens after the child exits successfully, so only the output
	// failure determines the result returned by the public Command.Run API.
	cause := errors.New("output storage unavailable")
	pathErr := &os.PathError{Op: "write", Path: "spill", Err: cause}
	flushSpillFile = func(*bufio.Writer) error { return pathErr }
	command := NewCommand(1, "echo output", nil, 0)
	t.Cleanup(func() { _ = command.Cleanup() })
	out, err := command.Run(&Options{OutFileHandle: os.Stdout, ErrFileHandle: os.Stderr}, 1)
	for range out {
	}
	for _, got := range []error{err, command.Err} {
		var gotPath *os.PathError
		if !errors.Is(got, cause) || !errors.As(got, &gotPath) || gotPath != pathErr {
			t.Errorf("output error lost its cause or path: %v", got)
		}
	}
}

type errorReader struct{ err error }

func (r errorReader) Read([]byte) (int, error) { return 0, r.err }

type errorWriter struct{ err error }

func (w errorWriter) Write([]byte) (int, error) { return 0, w.err }

func TestOutputWritersRejectShortWritesAndErrors(t *testing.T) {
	sentinel := errors.New("write fault")
	for _, tt := range []struct {
		name string
		dst  io.Writer
		want error
	}{
		{name: "short write", dst: shortWriter{}, want: io.ErrShortWrite},
		{name: "write error", dst: errorWriter{sentinel}, want: sentinel},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var mu sync.Mutex
			w := &lockedWriter{mu: &mu, dst: tt.dst}
			_, _ = w.Write([]byte("abcdef"))
			if err := w.Error(); !errors.Is(err, tt.want) {
				t.Fatalf("error=%v; want %v", err, tt.want)
			}
		})
	}
}

func TestSpillWriteFailureStopsRunAndRemovesTemp(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	withSpillSeams(t)
	oldLimit := TmpOutputDataBuffer
	TmpOutputDataBuffer = 0
	t.Cleanup(func() { TmpOutputDataBuffer = oldLimit })
	sentinel := errors.New("spill write fault")
	var spillPath string
	createSpillFile = func() (*os.File, error) {
		f, err := os.CreateTemp(t.TempDir(), "spill")
		if err == nil {
			spillPath = f.Name()
		}
		return f, err
	}
	writeSpillFile = func(*bufio.Writer, []byte) (int, error) { return 0, sentinel }

	state, ctx := runstate.New(context.Background())
	input := make(chan string, 2)
	input <- "echo output"
	input <- "echo queued"
	close(input)
	opts := &Options{Jobs: 1, OutFileHandle: os.Stdout, ErrFileHandle: os.Stderr, PropExitStatus: true}
	out, success, done, statuses := Run4OutputContext(opts, ctx, state.Cancel, input)

	// Add timeout to prevent hanging on macOS
	timeout := time.After(5 * time.Second)
	outDone := false
	successDone := false
	statusDone := false

drainLoop:
	for {
		select {
		case _, ok := <-out:
			if !ok {
				outDone = true
			}
		case _, ok := <-success:
			if !ok {
				successDone = true
			}
		case _, ok := <-statuses:
			if !ok {
				statusDone = true
			}
		case <-done:
			break drainLoop
		case <-timeout:
			t.Fatal("test timed out waiting for completion")
		}

		if outDone && successDone && statusDone {
			select {
			case <-done:
				break drainLoop
			case <-timeout:
				t.Fatal("test timed out waiting for done signal")
			}
		}
	}

	if cause := state.Cause(); cause.Kind != runstate.Internal || cause.Status != 1 {
		t.Fatalf("cause=%#v; want internal status 1", cause)
	}
	if spillPath == "" {
		t.Fatal("spill file was not created")
	}
	if _, err := os.Stat(spillPath); !os.IsNotExist(err) {
		t.Fatalf("spill file still exists: %v", err)
	}
}

type observedRun struct {
	commands   []*Command
	successful []string
	statuses   []int
	cause      runstate.Cause
}

func observeRun[T commandInput](t *testing.T, opts *Options, texts ...T) observedRun {
	t.Helper()
	state, ctx := runstate.New(context.Background())
	input := make(chan T, len(texts))
	for _, text := range texts {
		input <- text
	}
	close(input)
	commands, success, done, statuses := runContext(opts, ctx, state.Cancel, input)
	var observation observedRun
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		for command := range commands {
			for range command.Ch {
			}
			if err := command.Cleanup(); err != nil {
				t.Errorf("cleanup: %v", err)
			}
			observation.commands = append(observation.commands, command)
		}
	}()
	go func() {
		defer wg.Done()
		for command := range success {
			observation.successful = append(observation.successful, command)
		}
	}()
	go func() {
		defer wg.Done()
		for status := range statuses {
			observation.statuses = append(observation.statuses, status)
		}
	}()
	// Increase timeout on macOS where process cleanup is slower
	timeout := 3 * time.Second
	if runtime.GOOS == "darwin" {
		timeout = 6 * time.Second
	}
	select {
	case <-done:
	case <-time.After(timeout):
		t.Fatal("run did not finish")
	}
	wg.Wait()
	observation.cause = state.Cause()
	return observation
}

func TestSpillFinishFailureSurvivesTerminalErrors(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell terminal-status cases are compiled on Windows and exercised on Unix")
	}
	for _, tt := range []struct {
		name       string
		command    string
		timeout    time.Duration
		stopOnErr  bool
		wantStatus int
		wantCause  runstate.Cause
	}{
		{
			name:       "nonzero exit",
			command:    "printf output; exit 7",
			wantStatus: 7,
			wantCause:  runstate.Cause{Kind: runstate.Internal, Status: 1},
		},
		{
			name:       "timeout",
			command:    "printf output; while :; do :; done",
			timeout:    250 * time.Millisecond,
			wantStatus: 124,
			wantCause:  runstate.Cause{Kind: runstate.Internal, Status: 1},
		},
		{
			name:       "first terminal cause is preserved",
			command:    "printf output; exit 8",
			stopOnErr:  true,
			wantStatus: 8,
			wantCause:  runstate.Cause{Kind: runstate.StopOnError, Status: 8},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			withSpillSeams(t)
			oldLimit := TmpOutputDataBuffer
			TmpOutputDataBuffer = 0
			t.Cleanup(func() { TmpOutputDataBuffer = oldLimit })
			sentinel := errors.New("spill finish fault")
			flushSpillFile = func(*bufio.Writer) error { return sentinel }
			var spillPath string
			createSpillFile = func() (*os.File, error) {
				f, err := os.CreateTemp(t.TempDir(), "spill")
				if err == nil {
					spillPath = f.Name()
				}
				return f, err
			}
			opts := &Options{
				Jobs:                1,
				OutFileHandle:       os.Stdout,
				ErrFileHandle:       os.Stderr,
				PropExitStatus:      true,
				RecordSuccessfulCmd: true,
				StopOnErr:           tt.stopOnErr,
				Timeout:             tt.timeout,
			}
			got := observeRun(t, opts, tt.command)
			if got.cause != tt.wantCause {
				t.Fatalf("cause=%#v; want %#v", got.cause, tt.wantCause)
			}
			if len(got.statuses) != 1 || got.statuses[0] != tt.wantStatus {
				t.Fatalf("statuses=%v; want [%d]", got.statuses, tt.wantStatus)
			}
			if len(got.successful) != 0 {
				t.Fatalf("successful commands=%v; want none", got.successful)
			}
			var outputErr error
			if len(got.commands) == 1 {
				outputErr = got.commands[0].outputErr
			}
			if len(got.commands) != 1 || !errors.Is(outputErr, sentinel) {
				t.Fatalf("commands=%d output error=%v; want %v", len(got.commands), outputErr, sentinel)
			}
			if spillPath == "" {
				t.Fatal("spill file was not created")
			}
			if _, err := os.Stat(spillPath); !os.IsNotExist(err) {
				t.Fatalf("spill file still exists: %v", err)
			}
		})
	}
}

func TestBufferedStderrFailureSurvivesNonzeroExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell terminal-status case is compiled on Windows and exercised on Unix")
	}
	sentinelFile, err := os.CreateTemp(t.TempDir(), "read-only")
	if err != nil {
		t.Fatal(err)
	}
	name := sentinelFile.Name()
	if err := sentinelFile.Close(); err != nil {
		t.Fatal(err)
	}
	readOnly, err := os.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	defer readOnly.Close()
	opts := &Options{
		Jobs:                1,
		OutFileHandle:       os.Stdout,
		ErrFileHandle:       readOnly,
		PropExitStatus:      true,
		RecordSuccessfulCmd: true,
	}
	got := observeRun(t, opts, "printf error >&2; exit 9")
	if got.cause != (runstate.Cause{Kind: runstate.Internal, Status: 1}) {
		t.Fatalf("cause=%#v; want internal status 1", got.cause)
	}
	if len(got.statuses) != 1 || got.statuses[0] != 9 {
		t.Fatalf("statuses=%v; want [9]", got.statuses)
	}
	if len(got.successful) != 0 {
		t.Fatalf("successful commands=%v; want none", got.successful)
	}
	var outputErr error
	if len(got.commands) == 1 {
		outputErr = got.commands[0].outputErr
	}
	if len(got.commands) != 1 || outputErr == nil {
		t.Fatalf("commands=%d output error=%v; want stderr write failure", len(got.commands), outputErr)
	}
}

func TestOutputReaderFailureStopsQueuedScheduling(t *testing.T) {
	withSpillSeams(t)
	sentinel := errors.New("buffered output read fault")
	wrapSpillReader = func(io.Reader) io.Reader { return errorReader{err: sentinel} }
	opts := &Options{
		Jobs:                1,
		OutFileHandle:       os.Stdout,
		ErrFileHandle:       os.Stderr,
		PropExitStatus:      true,
		RecordSuccessfulCmd: true,
	}
	got := observeRun(t, opts, "echo first", "echo must-not-start")
	if got.cause != (runstate.Cause{Kind: runstate.Internal, Status: 1}) {
		t.Fatalf("cause=%#v; want internal status 1", got.cause)
	}
	var outputErr error
	if len(got.commands) == 1 {
		outputErr = got.commands[0].outputErr
	}
	if len(got.commands) != 1 || !errors.Is(outputErr, sentinel) {
		t.Fatalf("commands=%d output error=%v; want one reader failure", len(got.commands), outputErr)
	}
	if len(got.statuses) != 1 || got.statuses[0] != 1 {
		t.Fatalf("statuses=%v; want [1]", got.statuses)
	}
	if len(got.successful) != 0 {
		t.Fatalf("successful commands=%v; want none", got.successful)
	}
}

func TestOutputFailureWithETA(t *testing.T) {
	for _, failure := range []string{"read", "close"} {
		t.Run(failure, func(t *testing.T) {
			withSpillSeams(t)
			sentinel := fmt.Errorf("buffered output %s fault", failure)
			if failure == "read" {
				wrapSpillReader = func(io.Reader) io.Reader { return errorReader{err: sentinel} }
			} else {
				oldLimit := TmpOutputDataBuffer
				TmpOutputDataBuffer = 0
				t.Cleanup(func() { TmpOutputDataBuffer = oldLimit })
				closeSpillFile = func(file *os.File) error { return errors.Join(file.Close(), sentinel) }
			}
			state, ctx := runstate.New(context.Background())
			defer state.Cancel()
			bar := pb.NewOptions(1, pb.OptionSetWriter(io.Discard))
			opts := &Options{Jobs: 1, ETA: true, ETABar: bar, PropExitStatus: true, RecordSuccessfulCmd: true}
			input := make(chan string, 1)
			input <- "echo output"
			close(input)
			out, success, done, statuses := Run4OutputContext(opts, ctx, state.Cancel, input)
			for range out {
			}
			<-done
			if status, ok := <-statuses; !ok || status != 1 {
				t.Fatalf("status=%d, present=%t; want 1, true", status, ok)
			}
			if _, ok := <-statuses; ok {
				t.Fatal("duplicate job status")
			}
			if record, ok := <-success; ok {
				t.Fatalf("output failure recorded as successful: %q", record)
			}
			if cause := state.Cause(); cause != (runstate.Cause{Kind: runstate.Internal, Status: 1}) {
				t.Fatalf("cause=%+v; want internal status 1", cause)
			}
			if count := bar.State().CurrentNum; count != 1 {
				t.Fatalf("progress=%d; want one completed attempt", count)
			}
		})
	}
}

func TestSignalExclusionsIgnoreEmptyNames(t *testing.T) {
	for _, test := range []struct {
		name     string
		excluded []string
		allowed  bool
	}{
		{"", []string{""}, true},
		{"", []string{"unrelated", ""}, true},
		{"worker", []string{"", "unrelated", ""}, true},
		{"worker", []string{"", "WORKER", ""}, false},
		{"", []string{"", "all", ""}, false},
		{"worker", []string{"", "all", ""}, false},
	} {
		allowed, err := canSendSignal(test.name, test.excluded)
		if err != nil || allowed != test.allowed {
			t.Errorf("name=%q exclusions=%q: allowed=%t, err=%v; want %t", test.name, test.excluded, allowed, err, test.allowed)
		}
	}
}

func TestCommandRunRespondsToCancel(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("portable runtime helper is covered by the Windows console tests")
	}
	if runtime.GOOS == "darwin" {
		t.Skip("skipping on macOS: process cleanup is too slow even with CleanupTime=0 and SIGKILL")
	}
	cancel := make(chan struct{})
	c := NewCommand(1, "while :; do :; done", cancel, 0)
	opts := &Options{Jobs: 1, OutFileHandle: os.Stdout, ErrFileHandle: os.Stderr, CleanupTime: 0}
	done := make(chan error, 1)
	go func() {
		ch, err := c.Run(opts, 1)
		for range ch {
		}
		done <- err
	}()
	close(cancel)
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), ErrCancelled.Error()) {
			t.Fatalf("Run error = %v; want cancellation", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Command.Run did not respond to Cancel")
	}
}

func TestRunCancellationDoesNotBlockProducer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	commands := make(chan string)
	opts := &Options{Jobs: 1, OutFileHandle: os.Stdout, ErrFileHandle: os.Stderr}
	_, _, done, _ := Run4OutputContext(opts, ctx, cancel, commands)
	producerDone := make(chan struct{})
	go func() {
		defer close(producerDone)
		defer close(commands)
		for i := 0; i < 1000; i++ {
			select {
			case commands <- "echo x":
			case <-ctx.Done():
				return
			}
		}
	}()
	cancel()
	select {
	case <-producerDone:
	case <-time.After(time.Second):
		t.Fatal("producer remained blocked after cancellation")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("run supervisor did not finish after cancellation")
	}
}

type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) { return len(p) / 2, nil }

func TestWriteAllRejectsShortWrite(t *testing.T) {
	if err := writeAll(shortWriter{}, []byte("abcdef")); err != io.ErrShortWrite {
		t.Fatalf("writeAll error = %v; want %v", err, io.ErrShortWrite)
	}
}

func TestOutputFailureRetainsCleanupDiagnostics(t *testing.T) {
	for _, stage := range []string{"read", "flush", "retry"} {
		t.Run(stage, func(t *testing.T) {
			withSpillSeams(t)
			oldLimit, oldLog := TmpOutputDataBuffer, Log
			t.Cleanup(func() { TmpOutputDataBuffer, Log = oldLimit, oldLog })
			TmpOutputDataBuffer = 0
			var logs bytes.Buffer
			Log = logging.MustGetLogger("output-cleanup-errors")
			Log.SetBackend(logging.AddModuleLevel(logging.NewLogBackend(&logs, "", 0)))
			outputErr := fmt.Errorf("output %s failed", stage)
			previousErr := errors.New("previous retry output read failed")
			closeErr := errors.New("output close failed")
			removeErr := errors.New("output removal failed")
			dir := t.TempDir()
			createSpillFile = func() (*os.File, error) { return os.CreateTemp(dir, "output-") }
			if stage == "read" {
				wrapSpillReader = func(reader io.Reader) io.Reader {
					return io.MultiReader(reader, errorReader{err: outputErr})
				}
			} else if stage == "flush" {
				flushSpillFile = func(*bufio.Writer) error { return outputErr }
			} else {
				// The first attempt's read error is discovered only when the
				// final output is drained, after the second attempt's flush error.
				flushes := 0
				flushSpillFile = func(writer *bufio.Writer) error {
					flushes++
					if flushes == 1 {
						return writer.Flush()
					}
					return outputErr
				}
				wrapSpillReader = func(reader io.Reader) io.Reader {
					return io.MultiReader(reader, errorReader{err: previousErr})
				}
			}
			closeSpillFile = func(file *os.File) error { return errors.Join(file.Close(), closeErr) }
			removeSpillFile = func(string) error { return removeErr }
			opts := &Options{Jobs: 1, PropExitStatus: true, RecordSuccessfulCmd: true}
			command, status := "echo output", 1
			wantErrors := []error{outputErr, closeErr, removeErr}
			if stage == "retry" {
				opts.Retries = 1
				command, status = "echo output && exit 7", 7
				wantErrors = append(wantErrors, previousErr)
			}
			got := observeRun(t, opts, command)
			if got.cause != (runstate.Cause{Kind: runstate.Internal, Status: 1}) || len(got.statuses) != 1 || got.statuses[0] != status || len(got.successful) != 0 {
				t.Fatalf("cause=%+v statuses=%v successes=%v", got.cause, got.statuses, got.successful)
			}
			if len(got.commands) != 1 {
				t.Fatalf("commands=%d; want 1", len(got.commands))
			}
			for _, want := range wantErrors {
				if !errors.Is(got.commands[0].outputErr, want) {
					t.Errorf("lost %v from combined error: %v", want, got.commands[0].outputErr)
				}
				if !strings.Contains(logs.String(), want.Error()) {
					t.Errorf("missing %v in diagnostics: %q", want, logs.String())
				}
			}
		})
	}
}
