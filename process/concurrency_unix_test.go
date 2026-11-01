//go:build !windows

package process

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// registrationFailureController fails after the real stdout copier has opened
// a spill file, exercising cleanup after Start succeeds but registration fails.
type registrationFailureController struct {
	processController
	created <-chan *os.File
	file    *os.File
	err     error
}

func (c *registrationFailureController) Started(*exec.Cmd) error {
	select {
	case c.file = <-c.created:
		return c.err
	case <-time.After(time.Second):
		return errors.New("timed out waiting for spill file")
	}
}

func TestRegistrationFailureCleansSpill(t *testing.T) {
	for _, tc := range []struct{ gated, closeFails bool }{{false, false}, {true, false}, {false, true}} {
		t.Run(fmt.Sprintf("gated=%t/close-fails=%t", tc.gated, tc.closeFails), func(t *testing.T) {
			withSpillSeams(t)
			oldLimit, oldController := TmpOutputDataBuffer, makeProcessController
			t.Cleanup(func() { TmpOutputDataBuffer, makeProcessController = oldLimit, oldController })
			TmpOutputDataBuffer = 0
			dir := t.TempDir()
			created := make(chan *os.File, 1)
			createSpillFile = func() (*os.File, error) {
				file, err := os.CreateTemp(dir, "spill")
				created <- file
				return file, err
			}
			sentinel := errors.New("registration failed")
			closeErr := errors.New("spill close failed")
			if tc.closeFails {
				closeSpillFile = func(file *os.File) error {
					return errors.Join(file.Close(), closeErr)
				}
			}
			controller := &registrationFailureController{created: created, err: sentinel}
			makeProcessController = func(opts *Options) (processController, error) {
				inner, err := newPlatformProcessController(opts)
				controller.processController = inner
				return controller, err
			}
			opts := &Options{Jobs: 1, PropExitStatus: true}
			if tc.gated {
				opts.StartDelay = time.Millisecond
			}
			got := observeRun(t, opts, "printf spilled; while :; do :; done")
			if len(got.commands) != 1 || !errors.Is(got.commands[0].Err, sentinel) {
				t.Fatalf("commands=%v; want registration error", got.commands)
			}
			if tc.closeFails && (!errors.Is(got.commands[0].outputErr, closeErr) || got.cause.Status != 1) {
				t.Fatalf("output error=%v, cause=%v; cleanup error was lost", got.commands[0].outputErr, got.cause)
			}
			if controller.file == nil {
				t.Fatal("command did not create a spill file")
			}
			defer controller.file.Close()
			if _, err := controller.file.Stat(); !errors.Is(err, os.ErrClosed) {
				t.Errorf("spill descriptor not closed: %v", err)
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 0 {
				t.Fatalf("remaining spill files=%d, err=%v", len(entries), err)
			}
		})
	}
}

func TestKeepOrderPreservesWorkerConcurrency(t *testing.T) {
	dir := t.TempDir()
	release := dir + "/release"
	if err := syscall.Mkfifo(release, 0600); err != nil {
		t.Fatal(err)
	}
	payload := strings.Repeat("2", OutputChunkSize+1)
	input := make(chan string, 2)
	input <- fmt.Sprintf("read _ < %q; printf first", release)
	input <- "printf " + payload
	close(input)
	old := makeProcessController
	fake := &lifecycleController{events: make(chan string, 16)}
	makeProcessController = func(*Options) (processController, error) { return fake, nil }
	t.Cleanup(func() { makeProcessController = old })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opts := &Options{Jobs: 2, KeepOrder: true, OutFileHandle: os.Stdout, ErrFileHandle: os.Stderr}
	commands, _, done, _ := runContext(opts, ctx, cancel, input)
	started := 0
	deadline := time.After(2 * time.Second)
	for started < 2 {
		select {
		case event := <-fake.events:
			if event == "started" {
				started++
			}
		case <-deadline:
			t.Fatalf("started commands=%d; want 2", started)
		}
	}
	var ordered [2]*Command
	select {
	case command := <-commands:
		if command.ID != 2 {
			t.Fatalf("first completed command ID=%d; want 2", command.ID)
		}
		ordered[1] = command
	case <-time.After(2 * time.Second):
		t.Fatal("second command did not finish")
	}
	released := make(chan error, 1)
	go func() {
		f, err := os.OpenFile(release, os.O_WRONLY, 0)
		if err == nil {
			_, err = f.Write([]byte("go\n"))
			closeErr := f.Close()
			if err == nil {
				err = closeErr
			}
		}
		released <- err
	}()
	select {
	case err := <-released:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first command did not reach its release barrier")
	}
	select {
	case command := <-commands:
		if command.ID != 1 {
			t.Fatalf("second completed command ID=%d; want 1", command.ID)
		}
		ordered[0] = command
	case <-time.After(2 * time.Second):
		t.Fatal("supervisor blocked behind out-of-order multi-chunk output")
	}
	var got strings.Builder
	for _, command := range ordered {
		for chunk := range command.Ch {
			got.WriteString(chunk)
		}
		if err := command.Cleanup(); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("run did not complete after ordered output was drained")
	}
	want := "first" + payload
	if got.String() != want {
		t.Fatalf("output length=%d; want %d; ordered=%t", got.Len(), len(want), got.String() == want)
	}
}

type lifecycleController struct{ events chan string }

func (f *lifecycleController) Started(*exec.Cmd) error         { f.events <- "started"; return nil }
func (f *lifecycleController) Finished(*exec.Cmd)              { f.events <- "finished" }
func (f *lifecycleController) KillCommand(cmd *exec.Cmd) error { return cmd.Process.Kill() }
func (f *lifecycleController) StopAll(time.Duration, <-chan struct{}) error {
	f.events <- "stop"
	return nil
}
func (f *lifecycleController) Close() error { f.events <- "close"; return nil }

func TestControllerClosesAfterCommandsFinish(t *testing.T) {
	old := makeProcessController
	defer func() { makeProcessController = old }()
	fake := &lifecycleController{events: make(chan string, 8)}
	makeProcessController = func(*Options) (processController, error) { return fake, nil }
	input := make(chan string, 1)
	input <- "true"
	close(input)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opts := &Options{Jobs: 1, OutFileHandle: os.Stdout, ErrFileHandle: os.Stderr, PropExitStatus: true}
	out, success, done, status := Run4OutputContext(opts, ctx, cancel, input)
	for range out {
	}
	for range success {
	}
	for range status {
	}
	<-done
	close(fake.events)
	var events []string
	for e := range fake.events {
		events = append(events, e)
	}
	if strings.Join(events, ",") != "started,finished,close" {
		t.Fatalf("events=%v", events)
	}
}
