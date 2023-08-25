// +build windows

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
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"syscall"
	"unicode/utf16"
	"unsafe"

	"github.com/elastic/go-windows"
	"github.com/palantir/stacktrace"
	"github.com/pkg/errors"
)

var is32bitRush bool = runtime.GOARCH == "386"

var (
	modntdll                = syscall.NewLazyDLL("ntdll.dll")
	procNtReadVirtualMemory = modntdll.NewProc("NtReadVirtualMemory")

	modkernel32                  = syscall.NewLazyDLL("kernel32.dll")
	procCreateRemoteThread       = modkernel32.NewProc("CreateRemoteThread")
	procGetSystemWow64DirectoryW = modkernel32.NewProc("GetSystemWow64DirectoryW")
	procIsWow64Process           = modkernel32.NewProc("IsWow64Process")
	procOpenProcess              = modkernel32.NewProc("OpenProcess")
	procGetProcessTimes          = modkernel32.NewProc("GetProcessTimes")
	procVirtualAllocEx           = modkernel32.NewProc("VirtualAllocEx")
	procVirtualFreeEx            = modkernel32.NewProc("VirtualFreeEx")
	procVirtualQueryEx           = modkernel32.NewProc("VirtualQueryEx")
	procWaitForSingleObject      = modkernel32.NewProc("WaitForSingleObject")
	procWriteProcessMemory       = modkernel32.NewProc("WriteProcessMemory")

	// for VirtualAllocEx
	MEM_COMMIT             = 0x1000
	MEM_RESERVE            = 0x2000
	PAGE_EXECUTE_READWRITE = 0x40

	// for VirtualAllocFree
	MEM_RELEASE = 0x8000
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

const (
	PROCESS_CREATE_THREAD = 2
	PROCESS_VM_OPERATION  = 8
	PROCESS_VM_WRITE      = 32

	ERROR_ACCESS_DENIED     = 5
	ERROR_INVALID_PARAMETER = 87
)

func getNtErr(status uint32) error {
	ntStatus := windows.NTStatus(status)
	return stacktrace.PropagateWithCode(ntStatus, stacktrace.ErrorCode(ntStatus), "")
}

func getSyscallErr(e1 syscall.Errno) error {
	var simpleErr error = nil
	var errorCode int = 0
	if e1 != 0 {
		simpleErr = errnoErr(e1)
		errorCode = int(e1)
	} else {
		simpleErr = syscall.EINVAL
		errorCode = int(syscall.EINVAL)
	}
	return stacktrace.PropagateWithCode(simpleErr, stacktrace.ErrorCode(errorCode), "")
}

func isAccessDeniedError(err error) bool {
	if err != nil {
		return stacktrace.GetCode(err) == ERROR_ACCESS_DENIED
	} else {
		return false
	}
}

// from golang.org/x/sys/windows/zsyscall_windows.go
func openProcess(da uint32, inheritHandle bool, pid uint32) (handle syscall.Handle, err error) {
	var _p0 uint32
	if inheritHandle {
		_p0 = 1
	} else {
		_p0 = 0
	}
	r0, _, e1 := syscall.Syscall(procOpenProcess.Addr(), 3, uintptr(da), uintptr(_p0), uintptr(pid))
	handle = syscall.Handle(r0)
	if handle == 0 {
		err = getSyscallErr(e1)
	}
	return
}

func getProcessTimes(handle syscall.Handle, creationTime *syscall.Filetime, exitTime *syscall.Filetime, kernelTime *syscall.Filetime, userTime *syscall.Filetime) (err error) {
	r1, _, e1 := syscall.Syscall6(procGetProcessTimes.Addr(), 5, uintptr(handle), uintptr(unsafe.Pointer(creationTime)), uintptr(unsafe.Pointer(exitTime)), uintptr(unsafe.Pointer(kernelTime)), uintptr(unsafe.Pointer(userTime)), 0)
	if r1 == 0 {
		err = getSyscallErr(e1)
	}
	return
}

func getProcess(pid int) (processHandle int, processExists bool, accessGranted bool, err error) {
	processExists = false
	accessGranted = false
	var access uint32
	// All these are needed for CreateRemoteThread()
	// PROCESS_QUERY_INFORMATION is needed to call GetExitCodeProcess()
	// PROCESS_VM_READ is needed to read process memory
	access = PROCESS_CREATE_THREAD | syscall.PROCESS_QUERY_INFORMATION |
		PROCESS_VM_OPERATION | PROCESS_VM_WRITE | windows.PROCESS_VM_READ

	winProcessHandle, err := openProcess(
		access, false, uint32(pid))
	if winProcessHandle == syscall.InvalidHandle || winProcessHandle == 0 {
		processHandle = INVALID_HANDLE
	} else {
		processHandle = int(winProcessHandle)
	}
	if err == nil {
		processExists = true
		accessGranted = true
	} else {
		// ERROR_ACCESS_DENIED means that we don't have permission to open a handle to the process
		if !isAccessDeniedError(err) {
			accessGranted = true
			// ERROR_INVALID_PARAMETER means that the process does not exist
			if stacktrace.GetCode(err) != ERROR_INVALID_PARAMETER { // if process exists
				processExists = true
			}
		} else { // access denied
			// leave accessGranted = false, from initial value
			processExists = true
		}
	}
	return processHandle, processExists, accessGranted, err
}

func releaseProcessByHandle(processHandle int) {
	syscall.CloseHandle(syscall.Handle(processHandle))
}

func doesProcessExist(processHandle int) (processExists bool) {
	processExists = false
	var state uint32
	winProcessHandle := syscall.Handle(processHandle)
	err := syscall.GetExitCodeProcess(winProcessHandle, &state)
	if err == nil {
		if state == STILL_ACTIVE { // process still exists
			processExists = true
		}
	}
	return processExists
}

func _getProcessStartTime(processHandle int) (startTime uint64, err error) {
	var times syscall.Rusage
	err = getProcessTimes(
		syscall.Handle(processHandle),
		&times.CreationTime,
		&times.ExitTime,
		&times.KernelTime,
		&times.UserTime,
	)
	// from https://github.com/cloudfoundry/gosigar/blob/master/sigar_windows.go
	// Windows epoch times are expressed as time elapsed since midnight on
	// January 1, 1601 at Greenwich, England. This converts the Filetime to
	// unix epoch in milliseconds.
	startTime = uint64(times.CreationTime.Nanoseconds() / 1e6)
	return
}

func waitForSingleObject(handle syscall.Handle, waitMilliseconds uint32) (event uint32, err error) {
	r0, _, e1 := syscall.Syscall(procWaitForSingleObject.Addr(), 2, uintptr(handle), uintptr(waitMilliseconds), 0)
	event = uint32(r0)
	if event == 0xffffffff {
		err = getSyscallErr(e1)
	}
	return
}

func _ntReadVirtualMemory(handle syscall.Handle, baseAddress uintptr, buffer uintptr, size uintptr, numRead *uintptr) (ntStatus uint32) {
	r0, _, _ := procNtReadVirtualMemory.Call(
		uintptr(handle), uintptr(baseAddress), uintptr(buffer), uintptr(size), uintptr(unsafe.Pointer(numRead)))
	ntStatus = uint32(r0)
	return
}

func ntReadVirtualMemory(handle syscall.Handle, baseAddress uintptr, dest []byte) (numRead uintptr, err error) {
	n := len(dest)
	if n == 0 {
		return 0, nil
	}
	status := _ntReadVirtualMemory(handle, baseAddress, uintptr(unsafe.Pointer(&dest[0])), uintptr(n), &numRead)
	if status != 0 {
		return numRead, getNtErr(status)
	}
	return numRead, nil
}

func getProcessBasicInformation(processHandle syscall.Handle) (pbi windows.ProcessBasicInformationStruct, err error) {
	actualSize, err := windows.NtQueryInformationProcess(processHandle, windows.ProcessBasicInformation, unsafe.Pointer(&pbi), uint32(windows.SizeOfProcessBasicInformationStruct))
	if actualSize < uint32(windows.SizeOfProcessBasicInformationStruct) {
		return pbi, errors.New("Bad size for PROCESS_BASIC_INFORMATION")
	}
	return pbi, err
}

func getUserProcessParams(processHandle syscall.Handle, pbi windows.ProcessBasicInformationStruct) (params RtlUserProcessParameters, err error) {
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
	nRead, err := ntReadVirtualMemory(processHandle, pbi.PebBaseAddress, peb)
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
	nRead, err = ntReadVirtualMemory(processHandle, paramsAddr, paramsBuf)
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
func utf16BytesToString(b []byte) (string, error) {
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

func getSystemWow64DirectoryW(buffer uintptr, size uintptr) error {
	r1, _, e1 := procGetSystemWow64DirectoryW.Call(
		uintptr(buffer),
		uintptr(size))
	if int(r1) == 0 {
		return e1
	}
	return nil
}

func isWow64Process(processHandle syscall.Handle) (bool, error) {
	var wow64Process bool
	r1, _, e1 := procIsWow64Process.Call(
		uintptr(processHandle),
		uintptr(unsafe.Pointer(&wow64Process)))
	if int(r1) == 0 {
		return false, e1
	}
	return wow64Process, nil
}

// from https://github.com/contester/runlib/blob/master/win32/win32_windows.go
func _createRemoteThread(processHandle syscall.Handle, sa *syscall.SecurityAttributes, stackSize uint32, startAddress uintptr,
	parameter uintptr, creationFlags uint32) (syscall.Handle, uint32, error) {
	var threadId uint32
	r1, _, e1 := syscall.Syscall9(procCreateRemoteThread.Addr(),
		7,
		uintptr(processHandle),
		uintptr(unsafe.Pointer(sa)),
		uintptr(stackSize),
		startAddress,
		parameter,
		uintptr(creationFlags),
		uintptr(unsafe.Pointer(&threadId)),
		0,
		0)
	runtime.KeepAlive(sa)
	if int(r1) == 0 {
		return syscall.InvalidHandle, 0, getSyscallErr(e1)
	}
	return syscall.Handle(r1), threadId, nil
}

func _writeProcessMemory(handle syscall.Handle, baseAddress uintptr, buffer uintptr, size uintptr, numWritten *uintptr) (err error) {
	r1, _, e1 := syscall.Syscall6(procWriteProcessMemory.Addr(), 5, uintptr(handle), uintptr(baseAddress), uintptr(buffer), uintptr(size), uintptr(unsafe.Pointer(numWritten)), 0)
	if r1 == 0 {
		err = getSyscallErr(e1)
	}
	return
}

func _virtualAllocEx(processHandle syscall.Handle, address uintptr, size uintptr, allocType uint32, protect uint32) (baseAddr uintptr, err error) {
	r1, _, e1 := syscall.Syscall6(procVirtualAllocEx.Addr(), 5, uintptr(processHandle), uintptr(address), uintptr(size),
		uintptr(allocType), uintptr(protect), 0)
	if r1 == 0 {
		err = getSyscallErr(e1)
	}
	return r1, err
}

func _virtualFreeEx(processHandle syscall.Handle, address uintptr, size uintptr, freeType uint32) (err error) {
	r1, _, e1 := syscall.Syscall6(procVirtualFreeEx.Addr(), 4, uintptr(processHandle), uintptr(address), uintptr(size),
		uintptr(freeType), 0, 0)
	if r1 == 0 {
		err = getSyscallErr(e1)
	}
	return err
}

func _virtualQueryEx(processHandle syscall.Handle, address uintptr, buffer uintptr, size uintptr, nRead *uintptr) (err error) {
	r1, _, e1 := syscall.Syscall6(procVirtualQueryEx.Addr(), 4, uintptr(processHandle), uintptr(address), uintptr(buffer), uintptr(size),
		0, 0)
	if r1 == 0 {
		err = getSyscallErr(e1)
	}
	*nRead = r1
	return err
}

func virtualQueryEx(processHandle syscall.Handle, baseAddress uintptr, dest []byte) (nRead uintptr, err error) {
	n := len(dest)
	if n == 0 {
		return 0, nil
	}
	if err = _virtualQueryEx(processHandle, baseAddress, uintptr(unsafe.Pointer(&dest[0])), uintptr(n), &nRead); err != nil {
		return 0, err
	}
	return nRead, nil
}

func getProcessEnv(processHandle syscall.Handle, params RtlUserProcessParameters) (env string, err error) {
	// get the size of the env variables block
	mbiBuf := make([]byte, SizeOfMemoryBasicInformation)
	nRead, err := virtualQueryEx(processHandle, params.Environment, mbiBuf)
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
	nRead, err = windows.ReadProcessMemory(processHandle, params.Environment, envBuf)
	if err != nil {
		return env, err
	}
	if nRead != uintptr(envSize) {
		return env, errors.Errorf("Environment string: short read: (%d/%d)", nRead, envSize)
	}

	env, err = utf16BytesToString(envBuf)
	if err != nil {
		return env, err
	}

	return env, nil
}

func getShell() (shell string) {
	shell = os.Getenv("COMSPEC")
	if shell == "" {
		shell = "C:\\WINDOWS\\System32\\cmd.exe"
	}
	return shell
}

func getCommand(ctx context.Context, qcmd string) (command *exec.Cmd) {
	if ctx != nil {
		command = exec.CommandContext(ctx, getShell())
	} else {
		command = exec.Command(getShell())
	}
	// from https://github.com/junegunn/fzf/blob/390b49653b441c958b82a0f78d9923aef4c1d9a2/src/util/util_windows.go
	command.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    false,
		CmdLine:       fmt.Sprintf(` /s /c "%s"`, qcmd),
		CreationFlags: 0,
	}
	return command
}

func considerPid(pid int) bool {
	// skip our process, the System process, and the Idle process
	return pid != os.Getpid() &&
		pid != 0 &&
		pid != 4
}

func getSignalsToSend(childProcessName string, noStopExes []string, noKillExes []string) (signalsToSend int, err error) {
	signalsToSend = SEND_NO_SIGNAL // first assume no signal
	canSendStopSignal, err := canSendSignal(childProcessName, noStopExes)
	if err == nil {
		if canSendStopSignal {
			signalsToSend |= SEND_CTRL_C_SIGNAL
			signalsToSend |= SEND_CTRL_BREAK_SIGNAL
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
	err = nil
	hasMarker = false
	winProcessHandle := syscall.Handle(processHandle)
	pbi, err := getProcessBasicInformation(winProcessHandle)
	if err == nil {
		userProcParams, err := getUserProcessParams(winProcessHandle, pbi)
		if err == nil {
			env, err := getProcessEnv(winProcessHandle, userProcParams)
			if err == nil {
				hasMarker = containsMarker(env)
			}
		}
	}
	return hasMarker, err
}

func is64BitWindowsOS() bool {
	var wow64Dir [256]byte
	err1 := getSystemWow64DirectoryW(
		uintptr(unsafe.Pointer(&wow64Dir[0])),
		uintptr(len(wow64Dir)))
	return err1 == nil // no error means 64-bit Windows OS
}

/* C:\temp\shellcode\signalExe32.exe (9/20/2018 12:42:19 PM)
   StartOffset(h): 00000200, EndOffset(h): 000002B7, Length(h): 000000B8 */

var signalExe32 = []byte{
	0x83, 0xEC, 0x04, 0x56, 0x31, 0xC0, 0x31, 0xDB, 0xB3, 0x30, 0x64, 0x8B,
	0x03, 0x8B, 0x40, 0x0C, 0x8B, 0x40, 0x14, 0x50, 0x5E, 0x8B, 0x06, 0x50,
	0x5E, 0x8B, 0x06, 0x8B, 0x40, 0x10, 0x5E, 0xEB, 0x70, 0x60, 0x8B, 0x6C,
	0x24, 0x24, 0x8B, 0x45, 0x3C, 0x8B, 0x54, 0x05, 0x78, 0x01, 0xEA, 0x8B,
	0x4A, 0x18, 0x8B, 0x5A, 0x20, 0x01, 0xEB, 0xE3, 0x34, 0x49, 0x8B, 0x34,
	0x8B, 0x01, 0xEE, 0x31, 0xFF, 0x31, 0xC0, 0xFC, 0xAC, 0x84, 0xC0, 0x74,
	0x07, 0xC1, 0xCF, 0x0D, 0x01, 0xC7, 0xEB, 0xF4, 0x3B, 0x7C, 0x24, 0x28,
	0x75, 0xE1, 0x8B, 0x5A, 0x24, 0x01, 0xEB, 0x66, 0x8B, 0x0C, 0x4B, 0x8B,
	0x5A, 0x1C, 0x01, 0xEB, 0x8B, 0x04, 0x8B, 0x01, 0xE8, 0x89, 0x44, 0x24,
	0x1C, 0x61, 0xC3, 0xAD, 0x50, 0x52, 0xE8, 0xAA, 0xFF, 0xFF, 0xFF, 0x89,
	0x07, 0x83, 0xC4, 0x08, 0x83, 0xC7, 0x04, 0x39, 0xCE, 0x75, 0xEC, 0xC3,
	0xE8, 0x11, 0x00, 0x00, 0x00, 0x8E, 0x4E, 0x0E, 0xEC, 0xF9, 0x4C, 0xDC,
	0x3D, 0x83, 0xEC, 0x08, 0x89, 0xE5, 0x89, 0xC2, 0xEB, 0xEA, 0x5E, 0x8D,
	0x7D, 0x04, 0x89, 0xF1, 0x83, 0xC1, 0x08, 0xE8, 0xC7, 0xFF, 0xFF, 0xFF,
	0x6A, 0x00, 0x8B, 0x5C, 0x24, 0x14, 0x53, 0xFF, 0x55, 0x08, 0x31, 0xC0,
	0x83, 0xC4, 0x08, 0xC3}

/*
; from https://gist.github.com/debasishm89/5746556 with mods
; to call GenerateConsoleCtrlEvent
[Section .text]
[BITS 32]

start:
;===========FUNCTIONS=============
;=======Function : Get Kernel32 base address============
;Technique : PEB InMemoryOrderModuleList
sub esp, 0x4             ; reserve stack space for called functions
push esi
xor eax, eax             ; clear eax
xor ebx, ebx
mov bl,0x30
mov eax, [fs:ebx ]      ; get a pointer to the PEB
mov eax, [ eax + 0x0C ]  ; get PEB->Ldr
mov eax, [ eax + 0x14 ]  ; get PEB->Ldr.InMemoryOrderModuleList.Flink (1st entry)
push eax
pop esi
mov eax, [ esi ]         ; get the next entry (2nd entry)
push eax
pop esi
mov eax, [ esi ]         ; get the next entry (3rd entry)
mov eax, [ eax + 0x10 ]  ; get the 3rd entries base address (kernel32.dll)
pop esi

jmp start_main

;=======Function : Find function base address============
find_function:
pushad                           ;save all registers
mov ebp,  [esp  +  0x24]         ;put base address of module that is being
								 ;loaded in ebp
mov eax,  [ebp  +  0x3c]         ;skip over MSDOS header
mov edx,  [ebp  +  eax  +  0x78] ;go to export table and put relative address
								 ;in edx
add edx,  ebp                    ;add base address to it.
								 ;edx = absolute address of export table
mov ecx,  [edx  +  0x18]         ;set up counter ECX
								 ;(how many exported items are in array ?)
mov ebx,  [edx  +  0x20]         ;put names table relative offset in ebx
add ebx,  ebp                    ;add base address to it.
								 ;ebx = absolute address of names table

find_function_loop:
jecxz  find_function_finished    ;if ecx=0, then last symbol has been checked.
								 ;(should never happen)
								 ;unless function could not be found
dec ecx                          ;ecx=ecx-1
mov esi,  [ebx  +  ecx  *  4]    ;get relative offset of the name associated
								 ;with the current symbol
								 ;and store offset in esi
add esi,  ebp                    ;add base address.
								 ;esi = absolute address of current symbol

compute_hash:
xor edi,  edi                    ;zero out edi
xor eax,  eax                    ;zero out eax
cld                              ;clear direction flag.
								 ;will make sure that it increments instead of
								 ;decrements when using lods*

compute_hash_again:
lodsb                            ;load bytes at esi (current symbol name)
								 ;into al, + increment esi
test al,  al                      ;bitwise test :
								 ;see if end of string has been reached
jz  compute_hash_finished        ;if zero flag is set = end of string reached
ror edi,  0xd                    ;if zero flag is not set, rotate current
								 ;value of hash 13 bits to the right
add edi,  eax                    ;add current character of symbol name
								 ;to hash accumulator
jmp compute_hash_again           ;continue loop

compute_hash_finished:

find_function_compare:
cmp edi,  [esp  +  0x28]         ;see if computed hash matches requested hash (at esp+0x28)
								 ;edi = current computed hash
								 ;esi = current function name (string)
jnz find_function_loop           ;no match, go to next symbol
mov ebx,  [edx  +  0x24]         ;if match : extract ordinals table
								 ;relative offset and put in ebx
add ebx,  ebp                    ;add base address.
								 ;ebx = absolute address of ordinals address table
mov cx,  [ebx  +  2  *  ecx]     ;get current symbol ordinal number (2 bytes)
mov ebx,  [edx  +  0x1c]         ;get address table relative and put in ebx
add ebx,  ebp                    ;add base address.
								 ;ebx = absolute address of address table
mov eax,  [ebx  +  4  *  ecx]    ;get relative function offset from its ordinal and put in eax
add eax,  ebp                    ;add base address.
								 ;eax = absolute address of function address
mov [esp  +  0x1c],  eax         ;overwrite stack copy of eax so popad
								 ;will return function address in eax
find_function_finished:
popad                           ;retrieve original registers.
								;eax will contain function address
ret

;=======Function : loop to lookup functions for a given dll (process all hashes)============
find_funcs_for_dll:
	lodsd                   ;load current hash into eax (pointed to by esi)
	push eax                ;push hash to stack
	push edx                ;push base address of dll to stack
	call find_function
	mov [edi], eax          ;write function pointer into address at edi
	add esp, 0x08
	add edi, 0x04           ;increase edi to store next pointer
	cmp esi, ecx            ;did we process all hashes yet ?
	jne find_funcs_for_dll  ;get next hash and lookup function pointer
find_funcs_for_dll_finished:
	ret

;=======Function : Get pointers to function hashes============

GetHashes:
	call GetHashesReturn
  ;LoadLibraryA               hash 0xec0e4e8e number from rot13.exe
	db 0x8E
	db 0x4E
	db 0x0E
	db 0xEC

  ;GenerateConsoleCtrlEvent   hash 0x3ddc4cf9 number from rot13.exe
	db 0xF9
	db 0x4C
	db 0xDC
	db 0x3D

;====================================================================
;=================== MAIN APPLICATION ===============================
;====================================================================

start_main:
	sub esp,0x08         ;allocate space on stack to store 2 things :
						 ;in this order : ptr to LoadLibraryA, GenerateConsoleCtrlEvent
	mov ebp,esp          ;set ebp as frame ptr for relative offset
						 ;so we will be able to do this:
						 ;call ebp+4   = Execute LoadLibraryA
						 ;call ebp+8   = Execute GenerateConsoleCtrlEvent
	mov edx,eax      ;save base address of kernel32 in edx
;locate    functions inside kernel32 first
	jmp GetHashes    ;get address of first hash
GetHashesReturn:
	pop esi              ;get pointer to hash into esi
	lea edi, [ebp+0x4]   ;we will store the function addresses at edi
						 ; (edi will be increased with 0x04 for each hash)
						 ; (see resolve_symbols_for_dll)
	mov ecx,esi
	add ecx,0x08          ; store address of last hash into ecx
	call find_funcs_for_dll   ; get function pointers for the 2
							  ; kernel32 function hashes
						  ; and put them at ebp+4 and ebp+8

;GenerateConsoleCtrlEvent
	push 0               ; ProcessGroupId 0
	mov ebx, [esp+0x14]  ; get parameter from ThreadProc
	push ebx             ; CtrlEvent
	call [ebp+8]         ; GenerateConsoleCtrlEvent

	xor eax,eax          ; return 0 thread exit code (success)
	add esp, 0x8
	ret
*/

/* C:\temp\shellcode\signalExe64.exe (9/26/2018 10:17:38 AM)
   StartOffset(h): 00000200, EndOffset(h): 000002BA, Length(h): 000000BB */

var signalExe64 = []byte{
	0x49, 0x89, 0xC8, 0x48, 0x83, 0xEC, 0x28, 0x48, 0x83, 0xE4, 0xF0, 0x65,
	0x4C, 0x8B, 0x24, 0x25, 0x60, 0x00, 0x00, 0x00, 0x4D, 0x8B, 0x64, 0x24,
	0x18, 0x4D, 0x8B, 0x64, 0x24, 0x20, 0x4D, 0x8B, 0x24, 0x24, 0x4D, 0x8B,
	0x24, 0x24, 0x4D, 0x8B, 0x64, 0x24, 0x20, 0xBA, 0xF9, 0x4C, 0xDC, 0x3D,
	0x4C, 0x89, 0xE1, 0xE8, 0x11, 0x00, 0x00, 0x00, 0x48, 0x89, 0xC3, 0x31,
	0xD2, 0x44, 0x89, 0xC1, 0xFF, 0xD3, 0x31, 0xC0, 0x48, 0x83, 0xC4, 0x28,
	0xC3, 0x49, 0x89, 0xCD, 0x49, 0x0F, 0xB7, 0x45, 0x3C, 0x4D, 0x31, 0xF6,
	0x45, 0x8B, 0xB4, 0x05, 0x88, 0x00, 0x00, 0x00, 0x4D, 0x01, 0xEE, 0x4D,
	0x31, 0xD2, 0x45, 0x8B, 0x56, 0x18, 0x48, 0x31, 0xDB, 0x41, 0x8B, 0x5E,
	0x20, 0x4C, 0x01, 0xEB, 0x67, 0xE3, 0x47, 0x41, 0xFF, 0xCA, 0x48, 0x31,
	0xF6, 0x42, 0x8B, 0x34, 0x93, 0x4C, 0x01, 0xEE, 0x31, 0xFF, 0x31, 0xC0,
	0xFC, 0xAC, 0x84, 0xC0, 0x74, 0x07, 0xC1, 0xCF, 0x0D, 0x01, 0xC7, 0xEB,
	0xF4, 0x39, 0xD7, 0x75, 0xDB, 0x48, 0x31, 0xDB, 0x41, 0x8B, 0x5E, 0x24,
	0x4C, 0x01, 0xEB, 0x48, 0x31, 0xC9, 0x66, 0x42, 0x8B, 0x0C, 0x53, 0x48,
	0x31, 0xDB, 0x41, 0x8B, 0x5E, 0x1C, 0x4C, 0x01, 0xEB, 0x48, 0x31, 0xC0,
	0x8B, 0x04, 0x8B, 0x4C, 0x01, 0xE8, 0xC3}

/*
; from https://www.tophertimzen.com/blog/windowsx64Shellcode/ with mods
; to call GenerateConsoleCtrlEvent
bits 64
section .text
global start

start:
;get dll base addresses
	mov r8, rcx                  ;parameter from ThreadProc
	sub rsp, 28h                 ;reserve stack space for called functions
	and rsp, 0fffffffffffffff0h  ;make sure stack 16-byte aligned

	mov r12, [gs:60h]            ;peb
	mov r12, [r12 + 0x18]        ;Peb --> LDR
	mov r12, [r12 + 0x20]        ;Peb.Ldr.InMemoryOrderModuleList
	mov r12, [r12]               ;2st entry
	;[r12 + 0x20]                ;ntdll.dll base address!
	mov r12, [r12]               ;3nd entry
	mov r12, [r12 + 0x20]        ;kernel32.dll base address!

;get GenerateConsoleCtrlEvent address
	mov rdx, 0x3ddc4cf9          ;number from rot13.exe
	mov rcx, r12                 ;kernel32.dll
	call GetProcessAddress
	mov rbx, rax

;call GenerateConsoleCtrlEvent
	xor edx, edx                 ;ProcessGroupId 0
	mov ecx, r8d                 ;CtrlEvent (parameter from ThreadProc)
	call rbx

	xor eax, eax                 ;return 0 thread exit code (success)
	add rsp, 28h
	ret

;Hashing section to resolve a function address
GetProcessAddress:
	mov r13, rcx                   ;base address of dll loaded
	movzx rax, word [r13 + 0x3c]   ;skip DOS header and go to PE header
	xor r14, r14                   ;zero out r14
	mov r14d, [r13 + rax + 0x88]   ;0x88 offset from the PE header is the export table.

	add r14, r13                   ;make the export table an absolute base address and put it in r14.
	xor r10, r10                   ;zero out r10
	mov r10d, [r14 + 0x18]         ;go into the export table and get the numberOfNames
	xor rbx, rbx                   ;zero out rbx
	mov ebx, [r14 + 0x20]          ;get the AddressOfNames offset.
	add rbx, r13                   ;AddressofNames base.

find_function_loop:
	jecxz find_function_finished   ;if ecx is zero, quit :( nothing found.
	dec r10d                       ;dec ECX by one for the loop until a match/none are found
	xor rsi, rsi                   ;zero out rsi
	mov esi, [rbx + r10 * 4]       ;get a name to play with from the export table.
	add rsi, r13                   ;rsi is now the current name to search on.

find_hashes:
	xor edi, edi
	xor eax, eax
	cld

continue_hashing:
	lodsb                         ;get into al from rsi
	test al, al                   ;is the end of string resarched?
	jz compute_hash_finished
	ror dword edi, 0xd            ;ROR13 for hash calculation!
	add edi, eax
	jmp continue_hashing

compute_hash_finished:
	cmp edi, edx                  ;edx has the function hash
	jnz find_function_loop        ;didn't match, keep trying!
	xor rbx, rbx                  ;zero out rbx
	mov ebx, [r14 + 0x24]         ;put the address of the ordinal table and put it in ebx.
	add rbx, r13                  ;absolute address
	xor rcx, rcx                  ;zero out rcx
	mov cx, [rbx + 2 * r10]       ;ordinal = 2 bytes. Get the current ordinal and put it in cx. ECX was our counter for which # we were in.
	xor rbx, rbx                  ;zero out rbx
	mov ebx, [r14 + 0x1c]         ;extract the address table offset
	add rbx, r13                  ;put absolute address in rbx.
	xor rax, rax                  ;zero out rax
	mov eax, [rbx + 4 * rcx]      ;relative address
	add rax, r13

find_function_finished:
	ret
*/

func _signalProcess(processRecord ProcessRecord, signalNum int) (err error) {
	var winCtrlEvent int = 0
	switch signalNum {
	case CTRL_C_SIGNAL:
		winCtrlEvent = 0 // Windows CTRL_C_EVENT
	case CTRL_BREAK_SIGNAL:
		winCtrlEvent = 1 // Windows CTRL_BREAK_EVENT
	case KILL_SIGNAL:
		err = errors.New("Unexpected signalNum KILL_SIGNAL")
	default:
		err = errors.New("Unexpected signalNum")
	}

	var is32BitChildProcess bool = true
	if is64BitWindowsOS() {
		is32BitChildProcess, err = isWow64Process(syscall.Handle(processRecord.processHandle))
		if err != nil {
			if Verbose {
				Log.Error(err)
			}
		}
	}

	var signalExe []byte
	if is32BitChildProcess {
		signalExe = signalExe32
	} else {
		signalExe = signalExe64
	}

	signalExeLength := len(signalExe)
	var nWritten uintptr = 0
	remoteBaseAddr, err := _virtualAllocEx(
		syscall.Handle(processRecord.processHandle),
		0,
		uintptr(signalExeLength),
		uint32(MEM_COMMIT|MEM_RESERVE),
		uint32(PAGE_EXECUTE_READWRITE))
	if err == nil {
		err = _writeProcessMemory(
			syscall.Handle(processRecord.processHandle),
			remoteBaseAddr,
			uintptr(unsafe.Pointer(&signalExe[0])),
			uintptr(signalExeLength),
			&nWritten)
		if err != nil {
			if Verbose {
				if !isAccessDeniedError(err) {
					Log.Error(err)
				}
			}
		}
	} else {
		if Verbose {
			if !isAccessDeniedError(err) {
				Log.Error(err)
			}
		}
	}

	var threadHandle syscall.Handle = 0
	if err == nil {
		if nWritten == uintptr(signalExeLength) {
			threadHandle, _, err = _createRemoteThread(
				syscall.Handle(processRecord.processHandle), nil, 0, remoteBaseAddr, uintptr(winCtrlEvent), 0)
		} else {
			err = errors.Errorf("ThreadProc: short write (%d/%d)", nWritten, signalExeLength)
		}
	}

	if err == nil {
		if int(threadHandle) != 0 && int(threadHandle) != INVALID_HANDLE {
			if Verbose {
				msg := "sent "
				switch signalNum {
				case CTRL_C_SIGNAL:
					msg += "Ctrl+C"
				case CTRL_BREAK_SIGNAL:
					msg += "Ctrl+Break"
				case KILL_SIGNAL:
					err = errors.New("Unexpected signalNum KILL_SIGNAL")
				default:
					err = errors.New("Unexpected signalNum")
				}
				Log.Infof("%s to pid %s, waiting for response", msg, strconv.Itoa(processRecord.pid))
			}
			var event uint32
			event, err = waitForSingleObject(threadHandle, syscall.INFINITE)
			switch event {
			case syscall.WAIT_OBJECT_0:
				break
			case syscall.WAIT_FAILED:
				err = errors.New("Wait failed WaitForSingleObject")
			default:
				err = errors.New("Unexpected result from WaitForSingleObject")
			}
		} else {
			err = errors.New("Invalid thread handle from CreateRemoteThread")
		}
		if err != nil {
			if Verbose {
				if !isAccessDeniedError(err) {
					Log.Error(err)
				}
			}
		}
	} else {
		if Verbose {
			if !isAccessDeniedError(err) {
				Log.Error(err)
			}
		}
	}

	// ignore errors from CloseHandle, since process may have gone away
	syscall.CloseHandle(threadHandle)

	// ignore errors from free, since process may have gone away
	_virtualFreeEx(
		syscall.Handle(processRecord.processHandle),
		remoteBaseAddr,
		0,
		uint32(MEM_RELEASE))

	// ignore access denied error, since process may have gone away
	if isAccessDeniedError(err) {
		err = nil
	}
	return err
}

const (
	// from https://docs.microsoft.com/en-us/windows/desktop/api/processthreadsapi/nf-processthreadsapi-getexitcodeprocess
	STILL_ACTIVE = 259
)

func killProcess(processRecord ProcessRecord) (err error) {
	pidStr := strconv.Itoa(processRecord.pid)
	if Verbose {
		Log.Infof("taskkill /t /f /pid %s", pidStr)
	}
	out, err := exec.Command("taskkill", "/t", "/f", "/pid", pidStr).Output()
	if Verbose {
		if err != nil {
			Log.Error(err)
		}
		Log.Infof("%s", out)
	}
	return err
}

func signalProcess(processRecord ProcessRecord, signalNum int) (err error) {
	switch signalNum {
	case CTRL_C_SIGNAL, CTRL_BREAK_SIGNAL:
		err = _signalProcess(processRecord, signalNum)
	case KILL_SIGNAL:
		err = pollKillProcess(processRecord)
	default:
		err = errors.New("Unexpected signalNum")
	}
	return err
}

func canStopChildProcesses() bool {
	// some prerequisites prevent us from trying to stop the child processes
	// if so, print error to the user
	if is32bitRush && is64BitWindowsOS() {
		msg := "The win32 rush cannot signal 64-bit Windows child processes\n"
		msg += "       " // seven spaces indent
		msg += "Please use the win64 rush on 64-bit Windows"
		Log.Error(msg)
		return false
	} else {
		return true
	}
}
