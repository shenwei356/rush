// +build !windows

// Copyright © 2017 Wei Shen <shenwei356@gmail.com>
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
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
)

func getProcess(pid int) (processHandle int, processExists bool, accessGranted bool, err error) {
	// just use the pid as the process handle
	processHandle = pid
	// check if the process exists
	processExists = doesProcessExist(processHandle)
	// check if we have access to send a signal to the process
	pidStr := strconv.Itoa(pid)
	_, err = exec.Command("kill", "-0", pidStr).Output()
	if err == nil {
		accessGranted = true
	} else {
		accessGranted = false
	}
	return processHandle, processExists, accessGranted, err
}

func releaseProcess(processHandle int) {
	// nothing to release
}

func doesProcessExist(processHandle int) (processExists bool) {
	processExists = false
	// check if the process exists
	// just use the process handle as the pid
	pid := processHandle
	pidStr := strconv.Itoa(pid)
	_, err := exec.Command("ps", "-p", pidStr).Output()
	if err == nil {
		processExists = true
	}
	return processExists
}

func getShell() (shell string) {
	shell = os.Getenv("SHELL")
	if shell == "" {
		shell = "sh"
	}
	return shell
}

func getCommand(ctx context.Context, qcmd string) *exec.Cmd {
	if ctx != nil {
		return exec.CommandContext(ctx, getShell(), "-c", qcmd)
	} else {
		return exec.Command(getShell(), "-c", qcmd)
	}
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

func doesChildHaveMarker(processHandle int) (hasMarker bool, err error) {
	// the process handle is the pid
	envCmd := fmt.Sprintf("xargs -0 -n 1 < /proc/%d/environ", processHandle)
	env, err := exec.Command(getShell(), "-c", envCmd).Output()
	if err == nil {
		hasMarker = containsMarker(string(env))
	} else {
		hasMarker = false
	}
	return hasMarker, err
}

func _signalProcess(pid int, signalStr string) (err error) {
	pidStr := strconv.Itoa(pid)
	out, err := exec.Command("kill", signalStr, pidStr).Output()
	if Verbose {
		Log.Infof("ran kill %s %s, waiting for response", signalStr, pidStr)
		if err != nil {
			Log.Error(err)
		}
		Log.Infof("%s", out)
	}
	return err
}

func killProcess(processRecord ProcessRecord) error {
	// -9 means send kill signal
	return _signalProcess(processRecord.pid, "-9")
}

func signalProcess(processRecord ProcessRecord, signalNum int) (err error) {
	switch signalNum {
	case CTRL_C_SIGNAL:
		// -SIGINT means send Ctrl+C signal
		err = _signalProcess(processRecord.pid, "-SIGINT")
	case CTRL_BREAK_SIGNAL:
		// nothing to do since Ctrl+Break is Windows only
		err = nil
	case KILL_SIGNAL:
		err = pollKillProcess(processRecord)
	default:
		err = errors.New("Unexpected signalNum")
	}
	return err
}

func canStopChildProcesses() bool {
	return true
}
