//go:build !windows

package process

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// registrationFailureController fails after the real stdout copier has opened
// a spill file, exercising cleanup after Start succeeds but registration fails.
type registrationFailureController struct {
	processController
	created       <-chan *os.File
	file          *os.File
	err           error
	beforeFailure func() // Optional observation while the command is still owned and alive.
}

func (c *registrationFailureController) Started(*exec.Cmd) error {
	select {
	case c.file = <-c.created:
		if c.beforeFailure != nil {
			c.beforeFailure()
		}
		return c.err
	case <-time.After(time.Second):
		return errors.New("timed out waiting for spill file")
	}
}

func TestRegistrationFailureCleansSpillAndDescendants(t *testing.T) {
	for _, tc := range []struct{ gated, closeFails, exclusions, cleanupFails bool }{{}, {gated: true}, {closeFails: true}, {exclusions: true}, {cleanupFails: true}} {
		t.Run(fmt.Sprintf("gated=%t/close-fails=%t/exclusions=%t/cleanup-fails=%t", tc.gated, tc.closeFails, tc.exclusions, tc.cleanupFails), func(t *testing.T) {
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
			if tc.exclusions {
				opts.NoKillExes = []string{"unrelated-executable"}
			}
			pidFile := t.TempDir() + "/child"
			var childIdentity platformProcess
			controller.beforeFailure = func() {
				data, _ := os.ReadFile(pidFile)
				pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
				childIdentity, _ = lookupPlatformProcess(pid)
			}
			cleanupErr := errors.New("group cleanup failed")
			if tc.cleanupFails {
				oldSignal := sendUnixGroupSignal
				sendUnixGroupSignal = func(int, syscall.Signal) error { return cleanupErr }
				t.Cleanup(func() { sendUnixGroupSignal = oldSignal })
				opts.Retries = 1
			}
			// Publish the child PID before producing the output that triggers
			// the injected registration failure. Keep its pipes independent.
			command := fmt.Sprintf("sleep 30 >/dev/null 2>&1 & echo $! > %q; printf spilled; while :; do :; done", pidFile)
			t.Cleanup(func() {
				if childIdentity.identity != 0 {
					_ = signalUnixProcess(childIdentity, syscall.SIGKILL)
				}
			})
			commands := []string{command}
			if tc.cleanupFails {
				commands = append(commands, "echo must-not-start")
			}
			got := observeRun(t, opts, commands...)
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
			if tc.cleanupFails {
				if !errors.Is(got.commands[0].Err, cleanupErr) || got.cause.Status != 1 {
					t.Fatalf("cleanup failure was lost: err=%v cause=%v", got.commands[0].Err, got.cause)
				}
				return
			}
			data, err := os.ReadFile(pidFile)
			if err != nil {
				t.Fatal(err)
			}
			pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
			if err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(time.Second)
			for {
				child, err := lookupPlatformProcess(pid)
				if unixProcessGone(err) || (err == nil && child.zombie) {
					break
				}
				if err != nil || time.Now().After(deadline) {
					t.Fatalf("registration failure left child %d running: %v", pid, err)
				}
				time.Sleep(10 * time.Millisecond)
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

func TestUnregisteredCleanupHonorsChildExclusion(t *testing.T) {
	pidFile := t.TempDir() + "/child"
	cmd := getCommand(context.Background(), fmt.Sprintf("sleep 30 & echo $! > %q; wait", pidFile))
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	}()
	deadline := time.Now().Add(2 * time.Second)
	var child platformProcess
	for {
		data, _ := os.ReadFile(pidFile)
		pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
		p, err := lookupPlatformProcess(pid)
		if err == nil {
			name, err := fullPlatformProcessName(p)
			if err == nil && filepath.Base(name) == "sleep" {
				child = p
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("child did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := terminateUnregisteredCommand(cmd, &Options{NoKillExes: []string{"sleep"}, NoStopExes: []string{"sleep"}}); err != nil {
		t.Fatal(err)
	}
	current, err := lookupPlatformProcess(child.pid)
	if err != nil || current.identity != child.identity || current.zombie {
		t.Fatalf("excluded child was killed: %v", err)
	}
}

func TestUnregisteredCleanupAllowsGracefulStopWithoutKill(t *testing.T) {
	ready := t.TempDir() + "/ready"
	cmd := getCommand(context.Background(), fmt.Sprintf("trap 'exit 0' INT; echo ready > %q; while :; do :; done", ready))
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()
	deadline := time.Now().Add(time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("command did not install its signal handler")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := terminateUnregisteredCommand(cmd, &Options{NoKillExes: []string{"all"}}); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(time.Second)
	for {
		process, err := lookupPlatformProcess(cmd.Process.Pid)
		if unixProcessGone(err) || (err == nil && process.zombie) {
			break
		}
		if err != nil || time.Now().After(deadline) {
			t.Fatalf("allowed graceful signal was not delivered: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
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

func TestCommandRunDoesNotStartAfterCancellation(t *testing.T) {
	for _, duringSetup := range []bool{false, true} {
		t.Run(fmt.Sprintf("during-setup=%t", duringSetup), func(t *testing.T) {
			cancel := make(chan struct{})
			if !duringSetup {
				close(cancel)
			}
			old := makeProcessController
			fake := &lifecycleController{events: make(chan string, 16)}
			makeProcessController = func(*Options) (processController, error) {
				if duringSetup {
					close(cancel)
				}
				return fake, nil
			}
			t.Cleanup(func() { makeProcessController = old })
			command := NewCommand(1, "exec sleep 30", cancel, 0)
			out, err := command.Run(&Options{}, 1)
			for range out {
			}
			if !errors.Is(err, ErrCancelled) {
				t.Fatalf("Run error=%v; want cancellation", err)
			}
			close(fake.events)
			for event := range fake.events {
				if event == "started" {
					t.Fatal("started a process after cancellation")
				}
			}
			select {
			case _, open := <-command.Executed:
				if open {
					t.Fatal("cancelled command was marked executed")
				}
			default:
				t.Fatal("execution notification was not closed")
			}
		})
	}
}

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
