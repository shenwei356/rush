//go:build windows

package process

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const createNewProcessGroup = 0x00000200

func getShell() string { return "cmd" }
func getCommand(_ context.Context, qcmd string) *exec.Cmd {
	cmd := exec.Command(getShell(), "/d", "/s", "/c", qcmd)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNewProcessGroup}
	return cmd
}

type windowsProcess struct {
	pid, ppid uint32
	identity  uint64
	name      string
}
type windowsRecord struct {
	pid      uint32
	handle   windows.Handle
	identity uint64
	name     string
}
type windowsController struct {
	mu      sync.Mutex
	opts    *Options
	records map[*exec.Cmd]windowsRecord
	closed  bool
}

func newPlatformProcessController(opts *Options) (processController, error) {
	return &windowsController{opts: opts, records: make(map[*exec.Cmd]windowsRecord)}, nil
}
func processIdentity(handle windows.Handle) (uint64, error) {
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(handle, &created, &exited, &kernel, &user); err != nil {
		return 0, err
	}
	return uint64(created.HighDateTime)<<32 | uint64(created.LowDateTime), nil
}
func openIdentifiedProcess(pid uint32, access uint32) (windows.Handle, uint64, error) {
	h, err := windows.OpenProcess(access|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return 0, 0, err
	}
	id, err := processIdentity(h)
	if err != nil {
		windows.CloseHandle(h)
		return 0, 0, err
	}
	return h, id, nil
}
func (c *windowsController) Started(cmd *exec.Cmd) error {
	pid := uint32(cmd.Process.Pid)
	h, identity, err := openIdentifiedProcess(pid, windows.SYNCHRONIZE|windows.PROCESS_TERMINATE)
	if err != nil {
		return err
	}
	if identity == 0 {
		windows.CloseHandle(h)
		return fmt.Errorf("process %d has no creation identity", pid)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		windows.CloseHandle(h)
		return errors.New("process controller is closed")
	}
	c.records[cmd] = windowsRecord{pid: pid, handle: h, identity: identity, name: "cmd.exe"}
	return nil
}
func (c *windowsController) Finished(cmd *exec.Cmd) {
	c.mu.Lock()
	r, ok := c.records[cmd]
	delete(c.records, cmd)
	c.mu.Unlock()
	if ok {
		windows.CloseHandle(r.handle)
	}
}
func (c *windowsController) record(cmd *exec.Cmd) (windowsRecord, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.records[cmd]
	return r, ok
}
func (c *windowsController) KillCommand(cmd *exec.Cmd) error {
	r, ok := c.record(cmd)
	if !ok {
		return nil
	}
	return c.stop(r, c.opts.CleanupTime, c.opts.forceStopChannel())
}
func (c *windowsController) StopAll(cleanup time.Duration, force <-chan struct{}) error {
	c.mu.Lock()
	rs := make([]windowsRecord, 0, len(c.records))
	for _, r := range c.records {
		rs = append(rs, r)
	}
	c.mu.Unlock()
	errs := make(chan error, len(rs))
	var wg sync.WaitGroup
	for _, r := range rs {
		wg.Add(1)
		go func(r windowsRecord) { defer wg.Done(); errs <- c.stop(r, cleanup, force) }(r)
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
func (c *windowsController) validateRoot(r windowsRecord) error {
	id, err := processIdentity(r.handle)
	if err != nil {
		return err
	}
	if id != r.identity {
		return fmt.Errorf("process identity changed for pid %d", r.pid)
	}
	return nil
}
func snapshotWindowsProcesses() (map[uint32]windowsProcess, error) {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(snapshot)
	result := make(map[uint32]windowsProcess)
	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	err = windows.Process32First(snapshot, &entry)
	for err == nil {
		h, id, openErr := openIdentifiedProcess(entry.ProcessID, windows.SYNCHRONIZE)
		if openErr == nil {
			windows.CloseHandle(h)
			result[entry.ProcessID] = windowsProcess{pid: entry.ProcessID, ppid: entry.ParentProcessID, identity: id, name: windows.UTF16ToString(entry.ExeFile[:])}
		}
		entry.Size = uint32(unsafe.Sizeof(entry))
		err = windows.Process32Next(snapshot, &entry)
	}
	if !errors.Is(err, syscall.ERROR_NO_MORE_FILES) {
		return nil, err
	}
	return result, nil
}
func refreshWindowsTree(root windowsRecord, known map[uint32]windowsProcess) error {
	all, err := snapshotWindowsProcesses()
	if err != nil {
		return err
	}
	if p, ok := all[root.pid]; ok && p.identity != root.identity {
		return fmt.Errorf("process identity changed for pid %d", root.pid)
	}
	for {
		added := false
		for pid, p := range all {
			if p.identity == 0 {
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
func windowsProcessAlive(expected windowsProcess) (bool, error) {
	h, id, err := openIdentifiedProcess(expected.pid, windows.SYNCHRONIZE)
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer windows.CloseHandle(h)
	if id != expected.identity {
		return false, fmt.Errorf("process identity changed for pid %d", expected.pid)
	}
	status, err := windows.WaitForSingleObject(h, 0)
	if err != nil {
		return false, err
	}
	return status == uint32(windows.WAIT_TIMEOUT), nil
}
func refreshAndCheckWindowsTree(root windowsRecord, known map[uint32]windowsProcess) (bool, error) {
	if err := refreshWindowsTree(root, known); err != nil {
		return false, err
	}
	for _, p := range known {
		alive, err := windowsProcessAlive(p)
		if err != nil {
			return false, err
		}
		if alive {
			return true, nil
		}
	}
	return false, nil
}
func (c *windowsController) stop(root windowsRecord, cleanup time.Duration, force <-chan struct{}) error {
	if err := c.validateRoot(root); err != nil {
		return err
	}
	known := map[uint32]windowsProcess{root.pid: {pid: root.pid, identity: root.identity, name: root.name}}
	if err := refreshWindowsTree(root, known); err != nil {
		return err
	}
	var gracefulErr error
	sendBreak, _ := canSendSignal(root.name, c.opts.NoStopExes)
	if sendBreak {
		for _, p := range known {
			ok, _ := canSendSignal(p.name, c.opts.NoStopExes)
			if !ok {
				sendBreak = false
				break
			}
		}
	}
	if sendBreak {
		gracefulErr = windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, root.pid)
	}
	if cleanup > 0 {
		timer, ticker := time.NewTimer(cleanup), time.NewTicker(20*time.Millisecond)
		defer timer.Stop()
		defer ticker.Stop()
		for {
			alive, err := refreshAndCheckWindowsTree(root, known)
			if err != nil {
				return err
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
	forceErr := c.forceTree(root, known)
	if forceErr != nil {
		return forceErr
	}
	return gracefulErr
}
func (c *windowsController) forceTree(root windowsRecord, known map[uint32]windowsProcess) error {
	deadline := time.Now().Add(time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := refreshWindowsTree(root, known); err != nil {
			return err
		}
		ps := make([]windowsProcess, 0, len(known))
		for _, p := range known {
			ps = append(ps, p)
		}
		sort.Slice(ps, func(i, j int) bool { return ps[i].pid > ps[j].pid })
		eligibleAlive := false
		for _, p := range ps {
			kill, _ := canSendSignal(p.name, c.opts.NoKillExes)
			if !kill {
				continue
			}
			alive, err := windowsProcessAlive(p)
			if err != nil {
				return err
			}
			if !alive {
				continue
			}
			eligibleAlive = true
			h, id, err := openIdentifiedProcess(p.pid, windows.PROCESS_TERMINATE|windows.SYNCHRONIZE)
			if err != nil {
				return err
			}
			if id != p.identity {
				windows.CloseHandle(h)
				return fmt.Errorf("process identity changed for pid %d", p.pid)
			}
			err = windows.TerminateProcess(h, 1)
			windows.CloseHandle(h)
			if err != nil && !errors.Is(err, syscall.ERROR_ACCESS_DENIED) {
				return err
			}
		}
		if !eligibleAlive {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("forced cleanup exceeded one second for pid %d", root.pid)
		}
		<-ticker.C
	}
}
func (c *windowsController) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	for _, r := range c.records {
		windows.CloseHandle(r.handle)
	}
	c.records = nil
	return nil
}

func canStopChildProcesses() bool            { return true }
func considerPid(pid int) bool               { return pid > 0 }
func signalProcess(ProcessRecord, int) error { return nil }
func killProcess(ProcessRecord) error        { return nil }
func releaseProcessByHandle(int)             {}
func doesProcessExist(handle int) bool       { return handle != 0 }
