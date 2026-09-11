//go:build !windows
// +build !windows

// Copyright © 2017-2023 Wei Shen <shenwei356@gmail.com>
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package process

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"

	psutil "github.com/shirou/gopsutil/process"
)

func getProcess(pid int) (processHandle int, processExists bool, accessGranted bool, err error) {
	processHandle = INVALID_HANDLE
	err = syscall.Kill(pid, 0)
	if err == nil {
		processHandle = pid
		processExists = true
		accessGranted = true
	} else if err == syscall.EPERM {
		processExists = true
	}
	return processHandle, processExists, accessGranted, err
}

func releaseProcessByHandle(processHandle int) {
	// nothing to release
}

func doesProcessExist(processHandle int) (processExists bool) {
	err := syscall.Kill(processHandle, 0)
	return err == nil || err == syscall.EPERM
}

// from https://github.com/cloudfoundry/gosigar/blob/master/sigar_linux.go
var system struct {
	ticks uint64
	btime uint64
}

var Procd string

func readFile(file string, handler func(string) bool) error {
	contents, err := os.ReadFile(file)
	if err != nil {
		return err
	}

	reader := bufio.NewReader(bytes.NewBuffer(contents))

	for {
		line, _, err := reader.ReadLine()
		if err == io.EOF {
			break
		}
		if !handler(string(line)) {
			break
		}
	}

	return nil
}

func init() {
	system.ticks = 100 // C.sysconf(C._SC_CLK_TCK)

	Procd = "/proc"

	// grab system boot time
	readFile(Procd+"/stat", func(line string) bool {
		if strings.HasPrefix(line, "btime") {
			system.btime, _ = strtoull(line[6:])
			return false // stop reading
		}
		return true
	})
}

func strtoull(val string) (uint64, error) {
	return strconv.ParseUint(val, 10, 64)
}

func procFileName(pid int, name string) string {
	return Procd + "/" + strconv.Itoa(pid) + "/" + name
}

func readProcFile(pid int, name string) ([]byte, error) {
	path := procFileName(pid, name)
	contents, err := os.ReadFile(path)

	if err != nil {
		if perr, ok := err.(*os.PathError); ok {
			if perr.Err == syscall.ENOENT {
				return nil, syscall.ESRCH
			}
		}
	}

	return contents, err
}

func _getProcessStartTime(processHandle int) (startTime uint64, err error) {
	startTime = 0
	// just use the process handle as the pid
	pid := processHandle
	contents, err := readProcFile(pid, "stat")
	if err == nil {
		fields := strings.Fields(string(contents))

		// convert to millis
		startTime, _ = strtoull(fields[21])
		startTime /= system.ticks
		startTime += system.btime
		startTime *= 1000
	}
	return
}

func getShell() (shell string) {
	shell = os.Getenv("SHELL")
	if shell == "" {
		shell = "sh"
	}
	return shell
}

func getCommand(ctx context.Context, qcmd string) *exec.Cmd {
	var command *exec.Cmd
	if ctx != nil {
		command = exec.CommandContext(ctx, getShell(), "-c", qcmd)
	} else {
		command = exec.Command(getShell(), "-c", qcmd)
	}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if ctx != nil {
		command.Cancel = func() error {
			if command.Process == nil {
				return os.ErrProcessDone
			}
			err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
			if err == syscall.ESRCH {
				return os.ErrProcessDone
			}
			return err
		}
	}
	return command
}

func considerPid(pid int) bool {
	// skip our process and the init process
	return pid != os.Getpid() && pid != 1
}

func getSignalsToSend(childProcessName string, noStopExes []string, noKillExes []string) (signalsToSend int, err error) {
	signalsToSend = SEND_NO_SIGNAL // first assume no signal
	canSendStopSignal, err := canSendSignal(childProcessName, noStopExes)
	if err == nil {
		if canSendStopSignal {
			signalsToSend |= SEND_CTRL_C_SIGNAL
			// Ctrl+Break is Windows only, so it is not signaled
		}
		canSendKillSignal, err := canSendSignal(childProcessName, noKillExes)
		if err == nil {
			if canSendKillSignal {
				signalsToSend |= SEND_KILL_SIGNAL
			}
		}
	}
	return signalsToSend, err
}

func doesChildHaveMarker(_ *psutil.Process, processHandle int) (hasMarker bool, err error) {
	env, err := readProcFile(processHandle, "environ")
	if err == nil {
		hasMarker = containsMarker(string(env))
	} else {
		hasMarker = false
	}
	return hasMarker, err
}

func _signalProcess(pid int, signal syscall.Signal) (err error) {
	err = syscall.Kill(pid, signal)
	if Verbose {
		Log.Infof("sent %s to process %d", signal, pid)
		if err != nil {
			Log.Error(err)
		}
	}
	return err
}

func killProcess(processRecord ProcessRecord) error {
	return _signalProcess(processRecord.pid, syscall.SIGKILL)
}

func signalProcess(processRecord ProcessRecord, signalNum int) (err error) {
	switch signalNum {
	case CTRL_C_SIGNAL:
		err = _signalProcess(processRecord.pid, syscall.SIGINT)
	case CTRL_BREAK_SIGNAL:
		// nothing to do since Ctrl+Break is Windows only
		err = nil
	case KILL_SIGNAL:
		err = killProcess(processRecord)
	default:
		err = errors.New("Unexpected signalNum")
	}
	return err
}

func canStopChildProcesses() bool {
	return true
}
