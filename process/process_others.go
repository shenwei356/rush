//go:build !windows

package process

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"sync"
	"syscall"
	"time"
)

func getShell() string { return "sh" }
func getCommand(_ context.Context, qcmd string) *exec.Cmd {
	cmd := exec.Command(getShell(), "-c", qcmd)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd
}

type platformProcess struct {
	pid, ppid, pgid int
	identity        uint64
	name            string
	zombie          bool
}

type unixController struct {
	mu      sync.Mutex
	opts    *Options
	records map[*exec.Cmd]ProcessRecord
	closed  bool
}

func newPlatformProcessController(opts *Options) (processController, error) {
	return &unixController{opts: opts, records: make(map[*exec.Cmd]ProcessRecord)}, nil
}

func (c *unixController) Started(cmd *exec.Cmd) error {
	p, err := lookupPlatformProcess(cmd.Process.Pid)
	if err != nil {
		return fmt.Errorf("identify process %d: %w", cmd.Process.Pid, err)
	}
	if p.identity == 0 {
		return fmt.Errorf("process %d has no creation identity", p.pid)
	}
	if p.pgid != p.pid {
		return fmt.Errorf("unexpected process group %d for pid %d", p.pgid, p.pid)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return errors.New("process controller is closed")
	}
	c.records[cmd] = ProcessRecord{pid: p.pid, pgid: p.pgid, identity: p.identity, name: p.name, processExists: true, accessGranted: true}
	return nil
}

func (c *unixController) Finished(cmd *exec.Cmd) { c.mu.Lock(); delete(c.records, cmd); c.mu.Unlock() }
func (c *unixController) snapshot(cmd *exec.Cmd) (ProcessRecord, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.records[cmd]
	return r, ok
}
func (c *unixController) KillCommand(cmd *exec.Cmd) error {
	r, ok := c.snapshot(cmd)
	if !ok {
		return nil
	}
	return c.stop(r, c.opts.CleanupTime, c.opts.forceStopChannel())
}
func (c *unixController) StopAll(cleanup time.Duration, force <-chan struct{}) error {
	c.mu.Lock()
	records := make([]ProcessRecord, 0, len(c.records))
	for _, r := range c.records {
		records = append(records, r)
	}
	c.mu.Unlock()
	errs := make(chan error, len(records))
	var wg sync.WaitGroup
	for _, r := range records {
		wg.Add(1)
		go func(r ProcessRecord) { defer wg.Done(); errs <- c.stop(r, cleanup, force) }(r)
	}
	wg.Wait()
	close(errs)
	var first error
	for err := range errs {
		if err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (c *unixController) stop(root ProcessRecord, cleanup time.Duration, force <-chan struct{}) error {
	p, err := validateRootProcess(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	known := map[int]platformProcess{p.pid: p}
	if err := refreshUnixTree(root, known); err != nil {
		return err
	}
	if err := c.signalGraceful(root, known); err != nil {
		return err
	}
	if cleanup > 0 {
		timer, ticker := time.NewTimer(cleanup), time.NewTicker(20*time.Millisecond)
		defer timer.Stop()
		defer ticker.Stop()
		for {
			alive, err := refreshAndCheckUnixTree(root, known)
			if err != nil {
				return err
			}
			if !alive {
				return nil
			}
			select {
			case <-force:
				goto FORCE
			case <-timer.C:
				goto FORCE
			case <-ticker.C:
			}
		}
	}
FORCE:
	if err := refreshUnixTree(root, known); err != nil {
		return err
	}
	return c.signalForce(root, known)
}

func validateRootProcess(root ProcessRecord) (platformProcess, error) {
	p, err := lookupPlatformProcess(root.pid)
	if err != nil {
		return platformProcess{}, err
	}
	if p.identity != root.identity || p.pgid != root.pgid {
		return platformProcess{}, fmt.Errorf("process identity changed for pid %d", root.pid)
	}
	return p, nil
}

func refreshUnixTree(root ProcessRecord, known map[int]platformProcess) error {
	all, err := snapshotPlatformProcesses()
	if err != nil {
		return fmt.Errorf("enumerate descendants of %d: %w", root.pid, err)
	}
	if p, ok := all[root.pid]; ok && (p.identity != root.identity || p.pgid != root.pgid) {
		return fmt.Errorf("process identity changed for pid %d", root.pid)
	}
	for {
		added := false
		for pid, p := range all {
			if p.identity == 0 || p.zombie {
				continue
			}
			if old, ok := known[pid]; ok {
				if old.identity == p.identity {
					known[pid] = p
				}
				continue
			}
			if parent, ok := known[p.ppid]; ok {
				if current, exists := all[parent.pid]; !exists || current.identity == parent.identity {
					known[pid] = p
					added = true
				}
			}
		}
		if !added {
			break
		}
	}
	return nil
}

func refreshAndCheckUnixTree(root ProcessRecord, known map[int]platformProcess) (bool, error) {
	if err := refreshUnixTree(root, known); err != nil {
		return false, err
	}
	for pid, expected := range known {
		current, err := lookupPlatformProcess(pid)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, err
		}
		if current.identity == expected.identity && !current.zombie {
			return true, nil
		}
	}
	return false, nil
}

func (c *unixController) signalGraceful(root ProcessRecord, known map[int]platformProcess) error {
	if len(c.opts.NoStopExes) == 0 {
		if _, err := validateRootProcess(root); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if err := syscall.Kill(-root.pgid, syscall.SIGINT); err != nil && !errors.Is(err, syscall.ESRCH) {
			return err
		}
		for _, p := range known {
			if p.pgid != root.pgid {
				if err := signalUnixProcess(p, syscall.SIGINT); err != nil {
					return err
				}
			}
		}
		return nil
	}
	for _, p := range orderedUnixProcesses(known) {
		ok, _ := canSendSignal(p.name, c.opts.NoStopExes)
		if ok {
			if err := signalUnixProcess(p, syscall.SIGINT); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *unixController) signalForce(root ProcessRecord, known map[int]platformProcess) error {
	if len(c.opts.NoKillExes) == 0 {
		if _, err := validateRootProcess(root); err == nil {
			if err := syscall.Kill(-root.pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
				return err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		for _, p := range known {
			if p.pgid != root.pgid {
				if err := signalUnixProcess(p, syscall.SIGKILL); err != nil {
					return err
				}
			}
		}
		return nil
	}
	for _, p := range orderedUnixProcesses(known) {
		ok, _ := canSendSignal(p.name, c.opts.NoKillExes)
		if ok {
			if err := signalUnixProcess(p, syscall.SIGKILL); err != nil {
				return err
			}
		}
	}
	return nil
}

func orderedUnixProcesses(known map[int]platformProcess) []platformProcess {
	ps := make([]platformProcess, 0, len(known))
	for _, p := range known {
		ps = append(ps, p)
	}
	sort.Slice(ps, func(i, j int) bool { return ps[i].pid > ps[j].pid })
	return ps
}
func signalUnixProcess(expected platformProcess, sig syscall.Signal) error {
	current, err := lookupPlatformProcess(expected.pid)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if current.identity != expected.identity {
		return fmt.Errorf("process identity changed for pid %d", expected.pid)
	}
	if current.zombie {
		return nil
	}
	if err := syscall.Kill(expected.pid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}
func (c *unixController) Close() error {
	c.mu.Lock()
	c.closed = true
	c.records = nil
	c.mu.Unlock()
	return nil
}

func canStopChildProcesses() bool { return true }
func considerPid(pid int) bool    { return pid > 0 }
func signalProcess(r ProcessRecord, signalNum int) error {
	if signalNum == KILL_SIGNAL {
		return syscall.Kill(-r.pgid, syscall.SIGKILL)
	}
	return syscall.Kill(-r.pgid, syscall.SIGINT)
}
func killProcess(r ProcessRecord) error { return signalProcess(r, KILL_SIGNAL) }
func releaseProcessByHandle(int)        {}
func doesProcessExist(handle int) bool  { return handle > 0 }
