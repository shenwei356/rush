//go:build !windows

package process

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

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
