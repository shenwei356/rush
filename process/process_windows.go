// +build windows

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
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"

	"github.com/elastic/go-windows"
	"github.com/pkg/errors"
	psutil "github.com/shirou/gopsutil/process"
)

// from https://github.com/elastic/go-windows/zsyscall_windows.go
// Do the interface allocations only once for common
// Errno values.
const (
	errnoERROR_IO_PENDING = 997
)

var (
	errERROR_IO_PENDING error = syscall.Errno(errnoERROR_IO_PENDING)
)

// errnoErr returns common boxed Errno values, to prevent
// allocations at runtime.
func errnoErr(e syscall.Errno) error {
	switch e {
	case 0:
		return nil
	case errnoERROR_IO_PENDING:
		return errERROR_IO_PENDING
	}
	// TODO: add more here, after collecting data on the common
	// error values see on Windows. (perhaps when running
	// all.bat?)
	return e
}

func openProcess(pid int) (handle syscall.Handle, err error) {
	// PROCESS_QUERY_INFORMATION is needed to call GetExitCodeProcess()
	// PROCESS_VM_READ is needed to call ReadProcessMemory()
	handle, err = syscall.OpenProcess(
		syscall.PROCESS_QUERY_INFORMATION|windows.PROCESS_VM_READ, false, uint32(pid))
	if handle == 0 {
		handle = syscall.InvalidHandle
	}
	return handle, err
}

func getProcessBasicInformation(handle syscall.Handle) (pbi windows.ProcessBasicInformationStruct, err error) {
	actualSize, err := windows.NtQueryInformationProcess(handle, windows.ProcessBasicInformation, unsafe.Pointer(&pbi), uint32(windows.SizeOfProcessBasicInformationStruct))
	if actualSize < uint32(windows.SizeOfProcessBasicInformationStruct) {
		return pbi, errors.New("bad size for PROCESS_BASIC_INFORMATION")
	}
	return pbi, err
}

func getUserProcessParams(handle syscall.Handle, pbi windows.ProcessBasicInformationStruct) (params RtlUserProcessParameters, err error) {
	const is32bitProc = unsafe.Sizeof(uintptr(0)) == 4

	// Offset of params field within PEB structure.
	// This structure is different in 32 and 64 bit.
	paramsOffset := 0x20
	if is32bitProc {
		paramsOffset = 0x10
	}

	// Read the PEB from the target process memory
	pebSize := paramsOffset + 8
	peb := make([]byte, pebSize)
	nRead, err := windows.ReadProcessMemory(handle, pbi.PebBaseAddress, peb)
	if err != nil {
		return params, err
	}
	if nRead != uintptr(pebSize) {
		return params, errors.Errorf("PEB: short read (%d/%d)", nRead, pebSize)
	}

	// Get the RTL_USER_PROCESS_PARAMETERS struct pointer from the PEB
	paramsAddr := *(*uintptr)(unsafe.Pointer(&peb[paramsOffset]))

	// Read the RTL_USER_PROCESS_PARAMETERS from the target process memory
	paramsBuf := make([]byte, SizeOfRtlUserProcessParameters)
	nRead, err = windows.ReadProcessMemory(handle, paramsAddr, paramsBuf)
	if err != nil {
		return params, err
	}
	if nRead != uintptr(SizeOfRtlUserProcessParameters) {
		return params, errors.Errorf("RTL_USER_PROCESS_PARAMETERS: short read (%d/%d)", nRead, SizeOfRtlUserProcessParameters)
	}

	params = *(*RtlUserProcessParameters)(unsafe.Pointer(&paramsBuf[0]))
	return params, nil
}

// from https://github.com/elastic/go-windows/utf16.go, but without null terminal truncation
// UTF16BytesToString returns a string that is decoded from the UTF-16 bytes.
// The byte slice must be of even length otherwise an error will be returned.
func UTF16BytesToString(b []byte) (string, error) {
	if len(b)%2 != 0 {
		return "", fmt.Errorf("slice must have an even length (length=%d)", len(b))
	}

	s := make([]uint16, len(b)/2)
	for i := range s {
		s[i] = uint16(b[i*2]) + uint16(b[(i*2)+1])<<8
	}

	return string(utf16.Decode(s)), nil
}

// from https://github.com/elastic/go-windows/ntdll.go, but with Environment added
// RtlUserProcessParameters is Go's equivalent for the
// _RTL_USER_PROCESS_PARAMETERS struct.
// A few undocumented fields are exposed.
type RtlUserProcessParameters struct {
	Reserved1 [16]byte
	Reserved2 [5]uintptr

	// <undocumented>
	CurrentDirectoryPath   windows.UnicodeString
	CurrentDirectoryHandle uintptr
	DllPath                windows.UnicodeString
	// </undocumented>

	ImagePathName windows.UnicodeString
	CommandLine   windows.UnicodeString
	Environment   uintptr
}

// MemoryBasicInformation is Go's equivalent for the
// _MEMORY_BASIC_INFORMATION struct.
type MemoryBasicInformation struct {
	BaseAddress       uintptr
	AllocationBase    uintptr
	AllocationProtect uint32
	RegionSize        uintptr
	State             uint32
	Protect           uint32
	Type              uint32
}

const (
	// SizeOfRtlUserProcessParameters gives the size
	// of the RtlUserProcessParameters struct.
	SizeOfRtlUserProcessParameters = unsafe.Sizeof(RtlUserProcessParameters{})

	// SizeOfMemoryBasicInformation gives the size
	// of the MemoryBasicInformation struct.
	SizeOfMemoryBasicInformation = unsafe.Sizeof(MemoryBasicInformation{})
)

var (
	modkernel32        = syscall.NewLazyDLL("kernel32.dll")
	procVirtualQueryEx = modkernel32.NewProc("VirtualQueryEx")
)

func _VirtualQueryEx(handle syscall.Handle, address uintptr, buffer uintptr, size uintptr, nRead *uintptr) (err error) {
	r1, _, e1 := syscall.Syscall6(procVirtualQueryEx.Addr(), 4, uintptr(handle), uintptr(address), uintptr(buffer), uintptr(size),
		0, 0)
	if r1 == 0 {
		if e1 != 0 {
			err = errnoErr(e1)
		} else {
			err = syscall.EINVAL
		}
	}
	*nRead = r1
	return err
}

func VirtualQueryEx(handle syscall.Handle, baseAddress uintptr, dest []byte) (nRead uintptr, err error) {
	n := len(dest)
	if n == 0 {
		return 0, nil
	}
	if err = _VirtualQueryEx(handle, baseAddress, uintptr(unsafe.Pointer(&dest[0])), uintptr(n), &nRead); err != nil {
		return 0, err
	}
	return nRead, nil
}

func getProcessEnv(handle syscall.Handle, params RtlUserProcessParameters) (env string, err error) {
	// get the size of the env variables block
	mbiBuf := make([]byte, SizeOfMemoryBasicInformation)
	nRead, err := VirtualQueryEx(handle, params.Environment, mbiBuf)
	if err != nil {
		return env, err
	}
	if nRead != uintptr(SizeOfMemoryBasicInformation) {
		return env, errors.Errorf("MEMORY_BASIC_INFORMATION: short read (%d/%d)", nRead, SizeOfMemoryBasicInformation)
	}

	mbi := *(*MemoryBasicInformation)(unsafe.Pointer(&mbiBuf[0]))

	// read the content of the env variables block
	envSize := mbi.RegionSize - (uintptr(params.Environment) - uintptr(mbi.BaseAddress))

	envBuf := make([]byte, envSize)
	nRead, err = windows.ReadProcessMemory(handle, params.Environment, envBuf)
	if err != nil {
		return env, err
	}
	if nRead != uintptr(envSize) {
		return env, errors.Errorf("Environment string: short read: (%d/%d)", nRead, envSize)
	}

	env, err = UTF16BytesToString(envBuf)
	if err != nil {
		return env, err
	}

	return env, nil
}

func getChildMarkerKey() string {
	return "RUSH_CHILD_GROUP"
}

func getChildMarkerValue() string {
	// place brackets on either side of pid integer,
	// so we only find exact matches
	return fmt.Sprintf("[%d]", os.Getpid())
}

func getChildMarkerRegex() *regexp.Regexp {
	// match string with one or more [pid] values
	return regexp.MustCompile(getChildMarkerKey() + "=[\\[\\d+\\]]+")
}

func doesChildHaveMarker(handle syscall.Handle) (childHasMarker bool, err error) {
	childHasMarker = false
	pbi, err := getProcessBasicInformation(handle)
	if err == nil {
		userProcParams, err := getUserProcessParams(handle, pbi)
		if err == nil {
			env, err := getProcessEnv(handle, userProcParams)
			if err == nil {
				childMarkerRegex := getChildMarkerRegex()
				childMarkerValue := getChildMarkerValue()
				match := childMarkerRegex.FindString(env)
				childHasMarker = strings.Contains(match, childMarkerValue)
			}
		}
	}
	return childHasMarker, err
}

const (
	// from https://docs.microsoft.com/en-us/windows/desktop/api/processthreadsapi/nf-processthreadsapi-getexitcodeprocess
	STILL_ACTIVE = 259
)

func killWindowsChildProcess(handle syscall.Handle, pid int) (err error) {
	var state uint32
	err = syscall.GetExitCodeProcess(handle, &state)
	if err == nil {
		if state == STILL_ACTIVE { // process still exists
			attempts := 1
			for {
				if Verbose {
					Log.Infof("taskkill /t /f /pid %s", strconv.Itoa(pid))
				}

				out, err := exec.Command("taskkill", "/t", "/f", "/pid", strconv.Itoa(pid)).Output()

				if Verbose {
					if err != nil {
						Log.Error(err)
					}
					Log.Infof("%s", out)
				}

				err = syscall.GetExitCodeProcess(handle, &state)
				if err == nil {
					if state != STILL_ACTIVE { // process no longer exists
						break
					}
				} else {
					if Verbose {
						Log.Error(err)
					}
				}
				time.Sleep(10 * time.Millisecond)
				attempts += 1
				if attempts > 30 {
					// timed out
					timeoutMsg := fmt.Sprintf("Timed out trying to kill child process, pid %d", pid)
					err = errors.New(timeoutMsg)
					break
				}
			}
		}
	} else {
		if Verbose {
			Log.Error(err)
		}
	}
	return err
}

func checkWindowsChildProcess(childProcess *psutil.Process) (handle syscall.Handle, considerChild bool, err error) {
	// ignore errors from openProcess, since child may no longer exist
	var err2 error
	handle, err2 = openProcess(int(childProcess.Pid))
	if err2 == nil {
		if handle != syscall.InvalidHandle {
			considerChild, err = doesChildHaveMarker(handle)
		} else {
			// failed to open child process, so don't consider it
			considerChild = false
		}
	} else {
		// child no longer exists, so don't consider it
		considerChild = false
	}
	return handle, considerChild, err
}

// from https://softwareengineering.stackexchange.com/questions/177428/sets-data-structure-in-golang
type IntSet struct {
	set map[int]bool
}

func (set *IntSet) Add(i int) bool {
	_, found := set.set[i]
	set.set[i] = true
	return !found //False if it existed already
}

// stop process tree in bottom up order
func killWindowsProcessTreeRecursive(
	childProcess *psutil.Process,
	checkBeforeDescending bool,
	pidsVisited *IntSet,
) (childrenWereStopped bool) {
	childrenWereStopped = true // first assume true
	// skip our process, the System process, and the Idle process
	if childProcess.Pid != int32(os.Getpid()) &&
		childProcess.Pid != 0 &&
		childProcess.Pid != 4 {
		// avoid cycles in pid tree by looking at visited set
		if pidsVisited.Add(int(childProcess.Pid)) {
			handle := syscall.InvalidHandle
			considerChild := true // first assume true
			var err error
			if checkBeforeDescending {
				handle, considerChild, err = checkWindowsChildProcess(childProcess)
				if err != nil {
					childrenWereStopped = false
					if Verbose {
						Log.Error(err)
					}
				}
			}
			if considerChild {
				grandChildren, err := childProcess.Children()
				if err == nil {
					if grandChildren != nil {
						for _, grandChildProcess := range grandChildren {
							childrenWereStopped = childrenWereStopped && killWindowsProcessTreeRecursive(
								grandChildProcess,
								checkBeforeDescending,
								pidsVisited)
						}
					}
				} else {
					childrenWereStopped = false
					if Verbose {
						Log.Error(err)
					}
				}
				// only kill child process if we were able to kill its children
				if childrenWereStopped {
					if !checkBeforeDescending {
						handle, considerChild, err = checkWindowsChildProcess(childProcess)
						if err != nil {
							childrenWereStopped = false
							if Verbose {
								Log.Error(err)
							}
						}
					}
					if considerChild {
						if handle != syscall.InvalidHandle {
							err = killWindowsChildProcess(handle, int(childProcess.Pid))
							if err != nil {
								childrenWereStopped = false
								if Verbose {
									Log.Error(err)
								}
							}
						}
					}
				}
			}
			if handle != syscall.InvalidHandle {
				syscall.CloseHandle(handle)
			}
		}
	}
	return childrenWereStopped
}

// ensure our child processes go away
func KillWindowsChildProcesses() (childrenWereStopped bool) {
	// stop any child processes that were spawned from our env
	childrenWereStopped = true // first assume true
	processes, err := psutil.Processes()
	if err == nil {
		if processes != nil {
			pidsVisited := IntSet{set: make(map[int]bool)}
			for _, process := range processes {
				// specify checkBeforeDescending true here,
				// since only want to descend if process is one of our children
				childrenWereStopped = childrenWereStopped && killWindowsProcessTreeRecursive(
					process,
					true, // checkBeforeDescending
					&pidsVisited)
			}
		}
	} else {
		childrenWereStopped = false
		if Verbose {
			Log.Error(err)
		}
	}

	return childrenWereStopped
}

// from https://github.com/junegunn/fzf/blob/390b49653b441c958b82a0f78d9923aef4c1d9a2/src/util/util_windows.go
func (c *Command) setWindowsCommandFields(command *exec.Cmd, qcmd string) {
	if isWindows {
		command.SysProcAttr = &syscall.SysProcAttr{
			HideWindow:    false,
			CmdLine:       fmt.Sprintf(` /s /c "%s"`, qcmd),
			CreationFlags: 0,
		}
		// mark child processes with our pid,
		// so we can identify them later,
		// in case we need to kill them
		childMarkerKey := getChildMarkerKey()
		childMarkerValue := getChildMarkerValue()
		priorValue, found := os.LookupEnv(childMarkerKey)
		if found {
			// append marker values to sames key, so
			// we can handle the nested calls case
			childMarkerValue = priorValue + childMarkerValue
		}
		childMarker := fmt.Sprintf("%s=%s", childMarkerKey, childMarkerValue)
		// command de-dups variables, in favor of later values
		command.Env = append(os.Environ(), childMarker)
	} else {
		panic("should have called process_others.go setWindowsCommandFields()!")
	}
}
