package process

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shenwei356/rush/internal/runstate"
)

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

func observeRun(t *testing.T, opts *Options, texts ...string) observedRun {
	t.Helper()
	state, ctx := runstate.New(context.Background())
	input := make(chan string, len(texts))
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
	select {
	case <-done:
	case <-time.After(3 * time.Second):
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

func TestCommandRunRespondsToCancel(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("portable runtime helper is covered by the Windows console tests")
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
