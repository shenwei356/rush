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

// ownedCommandName follows exec replacements of the original shell process.
func ownedCommandName(cmd *exec.Cmd) (string, error) {
	p, err := lookupPlatformProcess(cmd.Process.Pid)
	if err != nil {
		return "", err
	}
	return fullPlatformProcessName(p)
}

type platformProcess struct {
	pid, ppid, pgid int
	identity        uint64
	name            string
	zombie          bool
}

var sendUnixGroupSignal = func(pgid int, sig syscall.Signal) error {
	return syscall.Kill(-pgid, sig)
}

// sendUnixProcessSignal is the checked per-process signal boundary.
var sendUnixProcessSignal = syscall.Kill

func unixProcessGone(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH)
}

// terminateUnregisteredCommand runs before Wait, while the unreaped child pins
// its PID. Its private process group can therefore be signaled without relying
// on the metadata lookup that may have caused registration to fail.
func terminateUnregisteredCommand(cmd *exec.Cmd, opts *Options) error {
	if !hasExecutableExclusions(opts.NoKillExes) {
		err := sendUnixGroupSignal(cmd.Process.Pid, syscall.SIGKILL)
		if err == nil || unixProcessGone(err) {
			return nil
		}
		if !errors.Is(err, syscall.EPERM) {
			return fmt.Errorf("kill unregistered process group %d: %w", cmd.Process.Pid, err)
		}
		// Group permission failures can still allow individual members to be
		// signaled. Reuse the identified-process fallback below.
	}
	p, err := lookupPlatformProcess(cmd.Process.Pid)
	if err != nil {
		return fmt.Errorf("identify unregistered process %d: %w", cmd.Process.Pid, err)
	}
	if p.identity == 0 || p.pgid != cmd.Process.Pid {
		return fmt.Errorf("invalid identity or group for unregistered process %d", cmd.Process.Pid)
	}
	root := ProcessRecord{pid: p.pid, pgid: p.pgid, identity: p.identity, name: p.name}
	controller := &unixController{opts: opts}
	if hasExecutableExclusions(opts.NoKillExes) {
		// Disabling forced termination must not suppress an allowed graceful
		// stop; otherwise Wait can hang on a process that would accept SIGINT.
		return controller.stop(root, opts.CleanupTime, opts.forceStopChannel())
	}
	known := map[int]platformProcess{p.pid: p}
	if err := refreshUnixTree(root, known); err != nil {
		return err
	}
	return controller.signalForce(root, known)
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
		if !unixProcessGone(err) {
			return fmt.Errorf("identify process %d: %w", cmd.Process.Pid, err)
		}
		// A short-lived shell can exit before inspection. Keep its process
		// group so a surviving child can still be stopped if necessary.
		p = platformProcess{pid: cmd.Process.Pid, pgid: cmd.Process.Pid}
	}
	if err == nil && p.identity == 0 {
		return fmt.Errorf("process %d has no creation identity", p.pid)
	}
	if p.pgid == 0 {
		return fmt.Errorf("process %d has no process group", p.pid)
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
	if err != nil && !unixProcessGone(err) {
		return fmt.Errorf("identify command process %d: %w", root.pid, err)
	}
	known := make(map[int]platformProcess)
	if err == nil {
		known[p.pid] = p
	} else {
		// The group may outlive its leader. Its ID cannot be reused while
		// members remain, so retain the members found before signaling it.
		all, scanErr := snapshotPlatformProcesses()
		if scanErr != nil {
			return scanErr
		}
		for pid, member := range all {
			if member.pgid == root.pgid && !member.zombie {
				known[pid] = member
			}
		}
		if len(known) == 0 {
			return nil
		}
	}
	if err := refreshUnixTree(root, known); err != nil {
		return err
	}
	// One denied member must not prevent escalation for other identified
	// processes. Retain the error while completing the allowed cleanup.
	gracefulErr := c.signalGraceful(root, known)
	if cleanup > 0 {
		timer, ticker := time.NewTimer(cleanup), time.NewTicker(20*time.Millisecond)
		defer timer.Stop()
		defer ticker.Stop()
		for {
			alive, err := refreshAndCheckUnixTree(root, known)
			if err != nil {
				gracefulErr = errors.Join(gracefulErr, err)
				goto FORCE
			}
			if !alive {
				return gracefulErr
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
		return errors.Join(gracefulErr, err)
	}
	return errors.Join(gracefulErr, c.signalForce(root, known))
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
		if unixProcessGone(err) {
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
	if allowed, _ := canSendSignal("", c.opts.NoStopExes); !allowed {
		return nil
	}
	var signalErr error // Collect member failures while visiting every safe target.
	if !hasExecutableExclusions(c.opts.NoStopExes) {
		alive, err := identifiedUnixGroup(root, known)
		if err != nil {
			return err
		}
		if alive {
			if err := signalKnownUnixGroup(root, known, syscall.SIGINT); err != nil {
				signalErr = errors.Join(signalErr, err)
			}
		}
		for _, p := range known {
			if p.pgid != root.pgid {
				if err := signalUnixProcess(p, syscall.SIGINT); err != nil {
					signalErr = errors.Join(signalErr, err)
				}
			}
		}
		return signalErr
	}
	for _, p := range orderedUnixProcesses(known) {
		name, err := fullPlatformProcessName(p)
		if unixProcessGone(err) {
			continue
		}
		if err != nil {
			signalErr = errors.Join(signalErr, fmt.Errorf("identify executable for pid %d: %w", p.pid, err))
			continue
		}
		ok, _ := canSendSignal(name, c.opts.NoStopExes)
		if ok {
			if err := signalUnixProcess(p, syscall.SIGINT); err != nil {
				signalErr = errors.Join(signalErr, err)
			}
		}
	}
	return signalErr
}

func (c *unixController) signalForce(root ProcessRecord, known map[int]platformProcess) error {
	if allowed, _ := canSendSignal("", c.opts.NoKillExes); !allowed {
		return nil
	}
	var signalErr error // Collect member failures while visiting every safe target.
	if !hasExecutableExclusions(c.opts.NoKillExes) {
		alive, err := identifiedUnixGroup(root, known)
		if err != nil {
			return err
		}
		if alive {
			if err := signalKnownUnixGroup(root, known, syscall.SIGKILL); err != nil {
				signalErr = errors.Join(signalErr, err)
			}
		}
		for _, p := range known {
			if p.pgid != root.pgid {
				if err := signalUnixProcess(p, syscall.SIGKILL); err != nil {
					signalErr = errors.Join(signalErr, err)
				}
			}
		}
		return signalErr
	}
	for _, p := range orderedUnixProcesses(known) {
		name, err := fullPlatformProcessName(p)
		if unixProcessGone(err) {
			continue
		}
		if err != nil {
			signalErr = errors.Join(signalErr, fmt.Errorf("identify executable for pid %d: %w", p.pid, err))
			continue
		}
		ok, _ := canSendSignal(name, c.opts.NoKillExes)
		if ok {
			if err := signalUnixProcess(p, syscall.SIGKILL); err != nil {
				signalErr = errors.Join(signalErr, err)
			}
		}
	}
	return signalErr
}

func identifiedUnixGroup(root ProcessRecord, known map[int]platformProcess) (bool, error) {
	if _, err := validateRootProcess(root); err == nil {
		return true, nil
	} else if !unixProcessGone(err) {
		return false, fmt.Errorf("identify process group %d: %w", root.pgid, err)
	}
	for _, member := range known {
		if member.pid == root.pid || member.pgid != root.pgid {
			continue
		}
		current, err := lookupPlatformProcess(member.pid)
		if unixProcessGone(err) {
			continue
		}
		if err != nil {
			return false, err
		}
		if current.identity == member.identity && current.pgid == root.pgid && !current.zombie {
			return true, nil
		}
	}
	return false, nil
}

func signalKnownUnixGroup(root ProcessRecord, known map[int]platformProcess, sig syscall.Signal) error {
	err := sendUnixGroupSignal(root.pgid, sig)
	if err == nil || errors.Is(err, syscall.ESRCH) {
		return nil
	}
	if !errors.Is(err, syscall.EPERM) {
		return fmt.Errorf("signal process group %d: %w", root.pgid, err)
	}
	// macOS may reject a group signal when any member has a different
	// effective UID. Signal the identified members that we can control.
	var memberErr error
	for _, p := range orderedUnixProcesses(known) {
		if p.pgid == root.pgid {
			memberErr = errors.Join(memberErr, signalUnixProcess(p, sig))
		}
	}
	if memberErr != nil {
		return fmt.Errorf("signal process group %d: %w", root.pgid, errors.Join(err, memberErr))
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
	if unixProcessGone(err) {
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
	if err := sendUnixProcessSignal(expected.pid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
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
