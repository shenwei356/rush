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
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cznic/sortutil"
	"github.com/pkg/errors"
	"github.com/shenwei356/go-logging"
	psutil "github.com/shirou/gopsutil/process"
)

// Log is *logging.Logger
var Log *logging.Logger

// pid_numSecondsSinceEpoch
var ChildMarker string = strconv.Itoa(os.Getpid()) + "_" + strconv.FormatInt(time.Now().Unix(), 16)

func init() {
	if Log == nil {
		logFormat := logging.MustStringFormatter(`%{color}[%{level:.4s}]%{color:reset} %{message}`)
		backend := logging.NewLogBackend(os.Stderr, "", 0)
		backendFormatter := logging.NewBackendFormatter(backend, logFormat)
		logging.SetBackend(backendFormatter)
		Log = logging.MustGetLogger("process")
	}
}

// Command is the Command struct
type Command struct {
	ID  uint64 // ID
	Cmd string // command

	Cancel    chan struct{}      // channel for close
	Timeout   time.Duration      // time out
	ctx       context.Context    // context.WithTimeout
	ctxCancel context.CancelFunc // cancel func for timetout

	Ch               chan string   // channel for stdout
	reader           *bufio.Reader // reader for stdout
	tmpfile          string        // tmpfile for stdout
	tmpfh            *os.File      // file handler for tmpfile
	finishSendOutput bool          // a flag of whether finished sending output to Ch

	Err      error         // Error
	Duration time.Duration // runtime

	dryrun     bool
	exitStatus int
}

// NewCommand create a Command
func NewCommand(id uint64, cmdStr string, cancel chan struct{}, timeout time.Duration) *Command {
	command := &Command{
		ID:      id,
		Cmd:     strings.TrimLeft(cmdStr, " "),
		Cancel:  cancel,
		Timeout: timeout,
	}
	return command
}

func (c *Command) String() string {
	return fmt.Sprintf("cmd #%d: %s", c.ID, c.Cmd)
}

// Verbose decides whether print extra information
var Verbose bool

var tmpfilePrefix = fmt.Sprintf("rush.%d.", os.Getpid())

// TmpOutputDataBuffer is buffer size for output of a command before saving to tmpfile,
// default 1M.
var TmpOutputDataBuffer = 1048576 // 1M

// OutputChunkSize is buffer size of output string chunk sent to channel, default 16K.
var OutputChunkSize = 16384 // 16K

// Run runs a command and send output to command.Ch in background.
func (c *Command) Run(opts *Options, tryNumber int) (chan string, error) {
	// create a return chan here; we will set the c.Ch in the parent
	ch := make(chan string, 1)

	if c.dryrun {
		ch <- c.Cmd + "\n"
		close(ch)
		c.finishSendOutput = true
		return ch, nil
	}

	c.Err = c.run(opts, tryNumber)

	// don't return here, keep going so we can display
	// the output from commands that error
	var readErr error = nil

	if Verbose {
		if c.exitStatus == 0 {
			Log.Infof("finish cmd #%d in %s: %s: exit status %d", c.ID, c.Duration, c.Cmd, c.exitStatus)
		} else {
			// exitStatus will appear in wait cmd message
			Log.Infof("finish cmd #%d in %s: %s", c.ID, c.Duration, c.Cmd)
		}
	}

	go func() {
		if opts.ImmediateOutput {
			close(ch)
			c.finishSendOutput = true
		} else {
			if c.tmpfile != "" { // data saved in tempfile
				c.reader = bufio.NewReader(c.tmpfh)
			}

			buf := make([]byte, OutputChunkSize)
			var n int
			var i int
			var b bytes.Buffer
			var bb []byte
			var existedN int
			// var N uint64
			for {
				if c.reader != nil {
					n, readErr = c.reader.Read(buf)
				} else {
					n = 0
					readErr = io.EOF
				}

				existedN = b.Len()
				b.Write(buf[0:n])

				if readErr != nil {
					if readErr == io.EOF {
						if b.Len() > 0 {
							// if Verbose {
							// 	N += uint64(b.Len())
							// }
							ch <- b.String() // string(buf[0:n])
						}
						b.Reset()
						readErr = nil
					}
					break
				}

				bb = b.Bytes()
				i = bytes.LastIndexByte(bb, '\n')
				if i < 0 {
					continue
				}

				// if Verbose {
				// 	N += uint64(len(bb[0 : i+1]))
				// }
				ch <- string(bb[0 : i+1]) // string(buf[0:n])

				b.Reset()
				if i-existedN+1 < n {
					// ------    ======i========n
					// existed   buf
					//   5          4      6
					b.Write(buf[i-existedN+1 : n])
				}
				// N += n
			}

			// if Verbose {
			// 	Log.Debugf("cmd #%d sent %d bytes\n", c.ID, N)
			// }

			// if Verbose {
			// 	Log.Infof("finish reading data from: %s", c.Cmd)
			// }

			close(ch)
			c.finishSendOutput = true
		}
	}()
	if c.Err != nil {
		return ch, c.Err
	} else {
		if readErr != nil {
			return ch, readErr
		} else {
			return ch, nil
		}
	}
}

// Cleanup removes tmpfile
func (c *Command) Cleanup() error {
	var err error
	if c.tmpfh != nil {
		// if Verbose {
		// 	Log.Infof("close tmpfh for: %s", c.Cmd)
		// }
		err = c.tmpfh.Close()
		if err != nil {
			return err
		}
	}

	if c.tmpfile != "" {
		if Verbose {
			Log.Infof("remove tmpfile (%s) for command: %s", c.tmpfile, c.Cmd)
		}
		err = os.Remove(c.tmpfile)
	}
	return err
}

// ErrTimeout means command timeout
var ErrTimeout = fmt.Errorf("time out")

// ErrCancelled means command being cancelled
var ErrCancelled = fmt.Errorf("cancelled")

func (c *Command) getExitStatus(err error) int {
	if exitError, ok := err.(*exec.ExitError); ok {
		waitStatus := exitError.Sys().(syscall.WaitStatus)
		return waitStatus.ExitStatus()
	}
	// no error, so return exitStatus 0
	return 0
}

type TopLevelEnum int

const (
	NotTopLevel TopLevelEnum = 0
	TopLevel    TopLevelEnum = 1
)

// lexicographically encode integer
// based on http://www.zanopha.com/docs/elen.pdf
func lexEncode(n uint64, topLevel TopLevelEnum) string {
	var encoded string
	// recursively calculate lex prefix
	// the lex prefix allows the user to lexicographically sort the output
	// need lex prefix if n has more than one digit
	nstr := fmt.Sprintf("%d", n)
	nlen := uint64(len(nstr))
	if nlen > 1 {
		// include non-numeric part of lex prefix
		// to allow proper sorting, this char must come after numerics in the ascii table
		encoded = fmt.Sprintf("_")
		// then include recursive part
		encoded += lexEncode(nlen, NotTopLevel)
		// conditionally include lex separator
		if topLevel == TopLevel {
			// the lex separator allows the user to differentiate a numeric part of the lex prefix from the original number
			// to allow proper sorting, the lex separator must come before numerics in the ascii table
			encoded += "."
		}
	}
	// include numeric part of lex prefix, or
	// original number (if topLevel==true)
	encoded += nstr
	return encoded
}

func getEntrySeparator() string {
	// to allow proper sorting, the entry separator must come before numerics in the ascii table
	return "/"
}

// ImmediateLineWriter is safe to use concurrently
type ImmediateLineWriter struct {
	lock          *sync.Mutex
	numJobs       int
	cmdId         uint64
	tryNumber     int
	line          string
	lineNumber    uint64
	includePrefix bool
}

func includeImmediatePrefix(cmdId uint64, tryNumber int, lineNumber uint64, data *string) {
	prefix := fmt.Sprintf("(%s", lexEncode(cmdId, TopLevel))
	prefix += fmt.Sprintf("%s%s", getEntrySeparator(), lexEncode(uint64(tryNumber), TopLevel))
	prefix += fmt.Sprintf("%s%s): ", getEntrySeparator(), lexEncode(lineNumber, TopLevel))
	if data != nil {
		*data = *data + prefix
	}
}

func NewImmediateLineWriter(lock *sync.Mutex, numJobs int, cmdId uint64, tryNumber int) *ImmediateLineWriter {
	lw := &ImmediateLineWriter{}
	lw.lock = lock
	lw.numJobs = numJobs
	lw.cmdId = cmdId
	lw.tryNumber = tryNumber
	lw.lineNumber = 1       // start with 1
	lw.includePrefix = true // start line 1 with a prefix
	return lw
}

func (lw *ImmediateLineWriter) addPrefixIfNeeded(output *string) {
	if lw.includePrefix {
		includeImmediatePrefix(lw.cmdId, lw.tryNumber, lw.lineNumber, output)
		lw.includePrefix = false
	}
}

func (lw *ImmediateLineWriter) WritePrefixedLines(input string, outfh *os.File) {
	if lw.lock != nil {
		// make immediate output thread-safe and do one write at a time
		lw.lock.Lock()
		// only include prefixes if jobs are running in parallel
		if lw.numJobs > 1 {
			var output string
			// split by \r\n or \n
			reg := regexp.MustCompile("(?:\r\n|\n)")
			matchExtents := reg.FindAllStringIndex(input, -1)
			if len(matchExtents) > 0 {
				// use runes below, so we work correctly with unicode strings
				rs := []rune(input)
				lastStart := 0
				for _, matchExtent := range matchExtents {
					beforePart := string(rs[lastStart:matchExtent[0]])
					lw.line = lw.line + beforePart
					// skip empty lines
					if len(lw.line) > 0 {
						// there is some data in this part, so add prefix if needed
						lw.addPrefixIfNeeded(&output)
						// append the chars up to and including the delimiter
						delimiterPart := string(rs[matchExtent[0]:matchExtent[1]])
						output = output + beforePart + delimiterPart
						// defer including prefix, so only add it on next non-empty data
						lw.includePrefix = true
						// clear line, since saw delimiter
						lw.line = ""
						lw.lineNumber++
					}
					lastStart = matchExtent[1]
				}
				// append any remaining chars after the last delimiter
				lastPart := string(rs[lastStart:len(rs)])
				if len(lastPart) > 0 {
					// there is some data in this part, so add prefix if needed
					lw.addPrefixIfNeeded(&output)
					lw.line = lw.line + lastPart
					output = output + lastPart
				}
			} else {
				// no delimiters in this section
				// there is some input, so add prefix if needed
				lw.addPrefixIfNeeded(&output)
				lw.line = lw.line + input
				output = output + input
			}
			if outfh != nil {
				outfh.WriteString(output)
			}
		} else {
			// no prefixes needed, since jobs are running serially
			// just use the input string
			if outfh != nil {
				outfh.WriteString(input)
			}
		}
		lw.lock.Unlock()
	}
}

type ImmediateWriter struct {
	lineWriter *ImmediateLineWriter
	fh         *os.File
}

func NewImmediateWriter(lineWriter *ImmediateLineWriter, fh *os.File) *ImmediateWriter {
	iw := &ImmediateWriter{}
	iw.lineWriter = lineWriter
	iw.fh = fh
	return iw
}

func (iw ImmediateWriter) Write(p []byte) (n int, err error) {
	dataLen := len(p)
	// only write non-empty data
	if dataLen > 0 {
		iw.lineWriter.WritePrefixedLines(string(p), iw.fh)
	}
	return dataLen, nil
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

const (
	INVALID_HANDLE int = 0

	CTRL_C_SIGNAL     int = 0
	CTRL_BREAK_SIGNAL int = 1
	KILL_SIGNAL       int = 2

	// bit mask
	SEND_NO_SIGNAL         int = 0
	SEND_CTRL_C_SIGNAL     int = 1
	SEND_CTRL_BREAK_SIGNAL int = 2
	SEND_KILL_SIGNAL       int = 4
)

func canSendSignal(childProcessName string, noSignalExes []string) (canSendSignal bool, err error) {
	canSendSignal = true // first assume true
	err = nil            // first assume no error
	if len(noSignalExes) > 0 {
		for _, noSignalExe := range noSignalExes {
			if noSignalExe == "all" {
				canSendSignal = false
				break
			} else {
				if childProcessName == noSignalExe {
					canSendSignal = false
					break
				}
			}
		}
	}
	return canSendSignal, err
}

func checkChildProcess(childProcess *psutil.Process, noStopExes []string, noKillExes []string) (processHandle int, considerChild bool, signalsToSend int, err error) {
	considerChild = false          // first assume false
	signalsToSend = SEND_NO_SIGNAL // first assume no signal
	// use err2 for getProcess, since child may no longer exist
	processHandle, processExists, accessGranted, err2 := getProcess(int(childProcess.Pid))
	if err2 == nil {
		if processHandle != INVALID_HANDLE {
			considerChild, err = doesChildHaveMarker(processHandle)
			if err == nil {
				if considerChild {
					var childProcessName string = ""
					if len(noStopExes) > 0 || len(noKillExes) > 0 {
						childProcessName, err = childProcess.Name()
						if err == nil {
							if len(childProcessName) == 0 {
								err = errors.New("childProcessName is empty")
							}
						}
						if err != nil {
							if Verbose {
								Log.Error(err)
							}
						}
					}
					signalsToSend, err = getSignalsToSend(childProcessName, noStopExes, noKillExes)
				}
			} else {
				if Verbose {
					Log.Error(err)
				}
			}
		} else {
			// failed to open child process, so don't consider it
		}
	} else {
		// failed to open child process, so don't consider it
		// check response
		if processExists {
			if accessGranted {
				// report errors from processes we could access
				if Verbose {
					Log.Error(err2)
				}
			} else { // access denied
				// ignore error, since we failed to get a handle to the child
				// it could be a system process that we are skipping anyway
			}
		} else { // process no longer exists
			// ignore error since no process to signal
		}
	}
	return processHandle, considerChild, signalsToSend, err
}

type ProcessRecord struct {
	processHandle int
	pid           int
	signalsToSend int
}

// get process tree in bottom up order
func getProcessTreeRecursive(
	childProcess *psutil.Process,
	noStopExes []string,
	noKillExes []string,
	pidsVisited *IntSet,
) (processRecords []ProcessRecord) {
	if considerPid(int(childProcess.Pid)) {
		// avoid cycles in pid tree by looking at visited set
		if pidsVisited.Add(int(childProcess.Pid)) {
			processHandle, considerChild, signalsToSend, err := checkChildProcess(childProcess, noStopExes, noKillExes)
			if err != nil {
				if Verbose {
					Log.Error(err)
				}
			}
			if processHandle != INVALID_HANDLE {
				if considerChild {
					grandChildren, err := childProcess.Children()
					if err != nil {
						if err == psutil.ErrorNoChildren {
							// ignore this error
							err = nil
						} else {
							if Verbose {
								Log.Error(err)
							}
						}
					} else {
						if grandChildren != nil {
							for _, grandChildProcess := range grandChildren {
								subProcessRecords := getProcessTreeRecursive(
									grandChildProcess,
									noStopExes,
									noKillExes,
									pidsVisited)
								for _, subProcessRecord := range subProcessRecords {
									processRecords = append(processRecords, subProcessRecord)
								}
							}
						}
					}
					var processRecord = ProcessRecord{
						processHandle: processHandle, pid: int(childProcess.Pid), signalsToSend: signalsToSend}
					processRecords = append(processRecords, processRecord)
				} else {
					releaseProcess(processHandle)
				}
			}
		}
	}
	return processRecords
}

func getChildProcesses(noStopExes []string, noKillExes []string) (processRecords []ProcessRecord) {
	// get any child processes that were spawned from our env
	processes, err := psutil.Processes()
	if err == nil {
		if processes != nil {
			pidsVisited := IntSet{set: make(map[int]bool)}
			for _, process := range processes {
				subProcessRecords := getProcessTreeRecursive(
					process,
					noStopExes,
					noKillExes,
					&pidsVisited)
				for _, subProcessRecord := range subProcessRecords {
					processRecords = append(processRecords, subProcessRecord)
				}
			}
		}
	} else {
		if Verbose {
			Log.Error(err)
		}
	}
	return processRecords
}

func signalChildProcesses(processRecords []ProcessRecord, signalNum int) (numChildrenSignaled int) {
	// signal child processes
	numChildrenSignaled = 0
	expectedNumChildrenSignaled := 0
	for _, processRecord := range processRecords {
		sendSignal := false // first assume false
		switch signalNum {
		case CTRL_C_SIGNAL:
			if processRecord.signalsToSend&SEND_CTRL_C_SIGNAL != 0 {
				sendSignal = true
			}
		case CTRL_BREAK_SIGNAL:
			if processRecord.signalsToSend&SEND_CTRL_BREAK_SIGNAL != 0 {
				sendSignal = true
			}
		case KILL_SIGNAL:
			if processRecord.signalsToSend&SEND_KILL_SIGNAL != 0 {
				sendSignal = true
			}
		default:
			Log.Error(errors.New("Unexpected signalNum"))
		}
		if sendSignal {
			expectedNumChildrenSignaled += 1
			err := signalProcess(processRecord, signalNum)
			if err == nil {
				numChildrenSignaled += 1
			} else {
				if Verbose {
					Log.Error(err)
				}
			}
		}
	}
	if expectedNumChildrenSignaled > 0 && numChildrenSignaled == 0 {
		switch signalNum {
		case CTRL_C_SIGNAL:
			Log.Error("no child processes sent Ctrl+C signal")
		case CTRL_BREAK_SIGNAL:
			Log.Error("no child processes sent Ctrl+Break signal")
		case KILL_SIGNAL:
			Log.Error("no child processes killed")
		default:
			Log.Error(errors.New("Unexpected signalNum"))
		}
	}
	return numChildrenSignaled
}

func anyRemainingChildren(processRecords []ProcessRecord) (anyRemaining bool) {
	anyRemaining = false
	for _, processRecord := range processRecords {
		if doesProcessExist(processRecord.processHandle) {
			anyRemaining = true
			break
		}
	}
	return anyRemaining
}

func pollRemainingChildren(processRecords []ProcessRecord, cleanupTime time.Duration) (anyRemaining bool) {
	anyRemaining = false
	startTime := time.Now()
	sleepTime := 250 * time.Millisecond
	for {
		continuePolling := false
		anyRemaining = anyRemainingChildren(processRecords)
		if anyRemaining && cleanupTime > 0 {
			time.Sleep(sleepTime)
			elapsedTime := time.Since(startTime)
			if elapsedTime < cleanupTime {
				// exponential back off with limit:
				// increase sleep time if next elapsedTime is below 1/2 of cleanupTime
				if elapsedTime+sleepTime*2 < cleanupTime/2 {
					// exponential back off
					sleepTime *= 2
				} else {
					// use the same sleepTime as before
				}
				continuePolling = true
			}
		}
		if !continuePolling {
			break
		}
	}
	return anyRemaining
}

func pollKillProcess(processRecord ProcessRecord) (err error) {
	if doesProcessExist(processRecord.processHandle) {
		attempts := 0
		for {
			continuePolling := false
			err = killProcess(processRecord)
			if doesProcessExist(processRecord.processHandle) {
				if attempts < 30 {
					continuePolling = true
				} else {
					// timed out
					err = errors.New(
						fmt.Sprintf("Timed out trying to kill child process, pid %d", processRecord.pid))
				}
			}
			if continuePolling {
				// don't use exponential back off here
				// since want to fail out after fixed number of attempts
				time.Sleep(250 * time.Millisecond)
				attempts += 1
			} else {
				break
			}
		}
	}
	return err
}

// ensure our child processes are stopped
func stopChildProcesses(noStopExes []string, noKillExes []string, cleanupTime time.Duration) (err error) {
	err = nil            // first assume no error
	anyRemaining := true // first assume some children
	totalNumSignaled := 0
	if canStopChildProcesses() {
		processRecords := getChildProcesses(noStopExes, noKillExes)
		// progress from most graceful to most invasive stop signal
		// if no matching children, then call is a noop
		numSignaled := signalChildProcesses(processRecords, CTRL_C_SIGNAL)
		if numSignaled > 0 {
			totalNumSignaled += numSignaled
			anyRemaining = pollRemainingChildren(processRecords, cleanupTime)
		} else {
			anyRemaining = true
		}
		if anyRemaining {
			numSignaled = signalChildProcesses(processRecords, CTRL_BREAK_SIGNAL)
			if numSignaled > 0 {
				totalNumSignaled += numSignaled
				anyRemaining = pollRemainingChildren(processRecords, cleanupTime)
			} else {
				anyRemaining = true
			}
			if anyRemaining {
				numSignaled = signalChildProcesses(processRecords, KILL_SIGNAL)
				totalNumSignaled += numSignaled
			}
		}
		anyRemaining = pollRemainingChildren(processRecords, 0) // wait zero time, since already waited above
		releaseProcesses(processRecords)
	}
	if anyRemaining && totalNumSignaled == 0 {
		msg := "No child processes stopped or killed\n"
		msg += "       " // seven spaces indent
		msg += "You will need to manually stop or kill them"
		err = errors.New(msg)
	}
	return err
}

func releaseProcesses(processRecords []ProcessRecord) {
	for _, processRecord := range processRecords {
		releaseProcess(processRecord.processHandle)
	}
}

func getChildMarkerKey() string {
	return "RUSH_CHILD_GROUP"
}

func getChildMarkerValue() string {
	// place brackets on either side of the marker,
	// so we only find exact matches
	return "[" + ChildMarker + "]"
}

func getChildMarkerRegex() *regexp.Regexp {
	// match string with one or more [pid_timestamp] values
	return regexp.MustCompile(getChildMarkerKey() + "=\\[[0-z]+\\]")
}

func containsMarker(env string) bool {
	childMarkerRegex := getChildMarkerRegex()
	childMarkerValue := getChildMarkerValue()
	match := childMarkerRegex.FindString(env)
	return strings.Contains(match, childMarkerValue)
}

var stopOnce sync.Once

// run a command and pass output to c.reader.
// Note that output returns only after finishing run.
// This function is mainly borrowed from https://github.com/brentp/gargs .
func (c *Command) run(opts *Options, tryNumber int) error {
	t := time.Now()
	chCancelMonitor := make(chan struct{})
	defer func() {
		close(chCancelMonitor)
		c.Duration = time.Now().Sub(t)
	}()

	var command *exec.Cmd
	qcmd := fmt.Sprintf(`%s`, c.Cmd)
	if Verbose {
		Log.Infof("start cmd #%d: %s", c.ID, qcmd)
	}

	if c.Timeout > 0 {
		c.ctx, c.ctxCancel = context.WithTimeout(context.Background(), c.Timeout)
		command = getCommand(c.ctx, qcmd)
	} else {
		command = getCommand(nil, qcmd)
	}

	// mark child processes with our pid,
	// so we can identify them later,
	// in case we need to signal them
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

	var pipeStdout io.ReadCloser = nil
	var err error = nil
	if opts.ImmediateOutput {
		lineWriter := NewImmediateLineWriter(&opts.ImmediateLock, opts.Jobs, c.ID, tryNumber)
		command.Stdout = NewImmediateWriter(lineWriter, opts.OutFileHandle)
		command.Stderr = NewImmediateWriter(lineWriter, opts.ErrFileHandle)
	} else {
		pipeStdout, err = command.StdoutPipe()
		if err != nil {
			return errors.Wrapf(err, "get stdout pipe of cmd #%d: %s", c.ID, c.Cmd)
		}
		// no code yet for stderr handling, so just have it go to os.Stderr
		command.Stderr = os.Stderr
	}

	err = command.Start()
	if err != nil {
		return errors.Wrapf(err, "start cmd #%d: %s", c.ID, c.Cmd)
	}

	var outPipe *bufio.Reader = nil
	if !opts.ImmediateOutput {
		outPipe = bufio.NewReaderSize(pipeStdout, TmpOutputDataBuffer)
		// no errPipe setting here, since having the command's stderr go to os.Stderr above
	}

	chErr := make(chan error, 2) // may come from three sources, must be buffered
	chEndBeforeTimeout := make(chan struct{})

	go func() {
		select {
		case <-c.Cancel:
			if Verbose {
				Log.Warningf("cancel cmd #%d: %s", c.ID, c.Cmd)
			}
			chErr <- ErrCancelled
			// ensure we only initiate the stop attempt once,
			// from all our command threads
			stopOnce.Do(func() {
				err = stopChildProcesses(opts.NoStopExes, opts.NoKillExes, opts.CleanupTime)
				if err != nil {
					if Verbose {
						Log.Error(err)
					}
					os.Exit(1)
				}
			})
		case <-chCancelMonitor:
			// default:  // must not use default, if you must use, use for loop
		}
	}()

	// detect timeout
	if c.Timeout > 0 {
		go func() { // goroutine #T
			select {
			case <-c.ctx.Done():
				chErr <- ErrTimeout
				c.ctxCancel()
				return
			case <-chEndBeforeTimeout:
				chErr <- nil
				return
			}
		}()
	}

	// --------------------------------

	// handle output
	var readed []byte

	if c.Timeout > 0 {
		// known shortcoming: this goroutine will remains even after timeout!
		// this will cause data race.
		go func() { // goroutine #P
			// Peek is blocked method, it waits command even after timeout!!
			if opts.ImmediateOutput {
				// set EOF here, since handling output in readLine() above
				err = io.EOF
			} else {
				readed, err = outPipe.Peek(TmpOutputDataBuffer)
			}
			chErr <- err
		}()
		err = <-chErr // from timeout #T or peek #P
	} else {
		if opts.ImmediateOutput {
			// set EOF here, since handling output in readLine() above
			err = io.EOF
		} else {
			readed, err = outPipe.Peek(TmpOutputDataBuffer)
		}
	}

	// less than TmpOutputDataBuffer bytes in output...
	if err == bufio.ErrBufferFull || err == io.EOF {
		if c.Timeout > 0 {
			go func() { // goroutine #W
				err1 := command.Wait()
				chErr <- err1
				close(chEndBeforeTimeout)
			}()
			err = <-chErr // from timeout #T or normal exit #W
			<-chErr       // from normal exit #W or timeout #T
		} else {
			err = command.Wait()
		}

		if opts.PropExitStatus {
			c.exitStatus = c.getExitStatus(err)
		}
		if !opts.ImmediateOutput {
			// get reader even on error, so we can still print the stdout and stderr of the failed child process
			c.reader = bufio.NewReader(bytes.NewReader(readed))
		}
		if err != nil {
			return errors.Wrapf(err, "wait cmd #%d: %s", c.ID, c.Cmd)
		}
		return nil
	}

	// more than TmpOutputDataBuffer bytes in output. must use tmpfile
	if opts.ImmediateOutput {
		panic("code assumes immediate output case does not use tmpfile")
	}
	if err != nil {
		return errors.Wrapf(err, "run cmd #%d: %s", c.ID, c.Cmd)
	}

	c.tmpfh, err = ioutil.TempFile("", tmpfilePrefix)
	if err != nil {
		return errors.Wrapf(err, "create tmpfile for cmd #%d: %s", c.ID, c.Cmd)
	}

	c.tmpfile = c.tmpfh.Name()

	if Verbose {
		Log.Infof("create tmpfile (%s) for command: %s", c.tmpfile, c.Cmd)
	}

	btmp := bufio.NewWriter(c.tmpfh)
	_, err = io.CopyBuffer(btmp, outPipe, readed)
	if err != nil {
		return errors.Wrapf(err, "save buffered data to tmpfile: %s", c.tmpfile)
	}

	if c, ok := pipeStdout.(io.ReadCloser); ok {
		c.Close()
	}
	btmp.Flush()
	_, err = c.tmpfh.Seek(0, 0)
	if err == nil {
		if c.Timeout > 0 {
			go func() { // goroutine #3
				err1 := command.Wait()
				close(chEndBeforeTimeout)
				chErr <- err1
			}()
			err = <-chErr // from timeout or normal exit
			<-chErr       // wait unfinished goroutine
		} else {
			err = command.Wait()
		}
	}
	if opts.PropExitStatus {
		c.exitStatus = c.getExitStatus(err)
	}
	if err != nil {
		return errors.Wrapf(err, "wait cmd #%d: %s", c.ID, c.Cmd)
	}

	return nil
}

// Options contains the options
type Options struct {
	DryRun              bool          // just print command
	Jobs                int           // max jobs number
	KeepOrder           bool          // keep output order
	Retries             int           // max retry chances
	RetryInterval       time.Duration // retry interval
	OutFileHandle       *os.File      // where to send stdout
	ErrFileHandle       *os.File      // where to send stderr
	ImmediateOutput     bool          // print output immediately and interleaved
	ImmediateLock       sync.Mutex    // make immediate output thread-safe and do one write at a time
	PrintRetryOutput    bool          // print output from retries
	Timeout             time.Duration // timeout
	StopOnErr           bool          // stop on any error
	NoStopExes          []string      // exe names to exclude from stop signal
	NoKillExes          []string      // exe names to exclude from kill signal
	CleanupTime         time.Duration // time to allow children to clean up
	PropExitStatus      bool          // propagate child exit status
	RecordSuccessfulCmd bool          // send successful command to channel
	Verbose             bool
}

// Run4Output runs commands in parallel from channel chCmdStr,
// and returns an output text channel,
// and a done channel to ensure safe exit.
func Run4Output(opts *Options, cancel chan struct{}, chCmdStr chan string) (chan string, chan string, chan int, chan int) {
	if opts.Verbose {
		Verbose = true
	}
	chCmd, chSuccessfulCmd, doneChCmd, chExitStatus := Run(opts, cancel, chCmdStr)
	chOut := make(chan string, opts.Jobs)
	done := make(chan int)

	go func() {
		var wg sync.WaitGroup

		if !opts.KeepOrder { // do not keep order
			tokens := make(chan int, opts.Jobs)

		RECEIVECMD:
			for c := range chCmd {
				select {
				case <-cancel:
					break RECEIVECMD
				default: // needed
				}

				wg.Add(1)
				tokens <- 1

				go func(c *Command) {
					defer func() {
						wg.Done()
						<-tokens
					}()

					// read data from channel and outpput
					// var N uint64
					for msg := range c.Ch {
						// if Verbose {
						// 	N += uint64(len(msg))
						// }
						chOut <- msg
					}
					c.Cleanup()

					// if Verbose {
					// 	Log.Debugf("receive %d bytes from cmd #%d\n", N, c.ID)
					// }
					// if Verbose {
					// 	Log.Infof("finish receiving data from: %s", c.Cmd)
					// }
				}(c)
			}

		} else { // keep order
			wg.Add(1)

			var id uint64 = 1
			var c, c1 *Command
			var ok bool
			cmds := make(map[uint64]*Command)
		RECEIVECMD2:
			for c = range chCmd {
				select {
				case <-cancel:
					if Verbose {
						Log.Debugf("cancel receiving finished cmd")
					}
					break RECEIVECMD2
				default: // needed
				}

				if c.ID == id { // your turn
					for msg := range c.Ch {
						chOut <- msg
					}
					c.Cleanup()

					id++
				} else { // wait the ID come out
					for {
						if c1, ok = cmds[id]; ok {
							for msg := range c1.Ch {
								chOut <- msg
							}
							c1.Cleanup()

							delete(cmds, c1.ID)
							id++
						} else {
							break
						}
					}
					cmds[c.ID] = c
				}
			}
			if len(cmds) > 0 {
				ids := make(sortutil.Uint64Slice, len(cmds))
				i := 0
				for id = range cmds {
					ids[i] = id
					i++
				}
				sort.Sort(ids)
				for _, id = range ids {
					c := cmds[id]
					for msg := range c.Ch {
						chOut <- msg
					}
					c.Cleanup()
				}
			}

			wg.Done()
		}

		<-doneChCmd
		wg.Wait()
		close(chOut)

		// if Verbose {
		// 	Log.Infof("finish sending all output")
		// }
		done <- 1
	}()
	return chOut, chSuccessfulCmd, done, chExitStatus
}

// write strings and report done
func combineWorker(input <-chan string, output chan<- string, wg *sync.WaitGroup) {
	defer wg.Done()
	for val := range input {
		output <- val
	}
}

// combine strings in input order
func combine(inputs []<-chan string, output chan<- string) {
	group := new(sync.WaitGroup)
	go func() {
		for _, input := range inputs {
			group.Add(1)
			go combineWorker(input, output, group)
			group.Wait() // preserve input order
		}
		close(output)
	}()
}

// Run runs commands in parallel from channel chCmdStr，
// and returns a Command channel,
// and a done channel to ensure safe exit.
func Run(opts *Options, cancel chan struct{}, chCmdStr chan string) (chan *Command, chan string, chan int, chan int) {
	if opts.Verbose {
		Verbose = true
	}

	chCmd := make(chan *Command, opts.Jobs)
	var chSuccessfulCmd chan string
	if opts.RecordSuccessfulCmd {
		chSuccessfulCmd = make(chan string, opts.Jobs)
	}
	done := make(chan int)
	var chExitStatus chan int
	if opts.PropExitStatus {
		chExitStatus = make(chan int, opts.Jobs)
	}

	go func() {
		var wg sync.WaitGroup
		tokens := make(chan int, opts.Jobs)
		var id uint64 = 1
		var stop bool
	RECEIVECMD:
		for cmdStr := range chCmdStr {
			select {
			case <-cancel:
				if Verbose {
					Log.Debugf("cancel receiving commands")
				}
				break RECEIVECMD
			default: // needed
			}

			if stop {
				break
			}

			wg.Add(1)
			tokens <- 1

			go func(id uint64, cmdStr string) {
				defer func() {
					wg.Done()
					<-tokens
				}()

				command := NewCommand(id, cmdStr, cancel, opts.Timeout)

				if opts.DryRun {
					command.dryrun = true
				}

				chances := opts.Retries
				var outputsToPrint []<-chan string
				for {
					tryNumber := opts.Retries - chances + 1
					ch, err := command.Run(opts, tryNumber)
					if err != nil { // fail to run
						if chances == 0 || opts.StopOnErr {
							// print final output
							outputsToPrint = append(outputsToPrint, ch)
							Log.Error(err)
							if opts.PropExitStatus {
								chExitStatus <- command.exitStatus
							}
							command.Ch = make(chan string, 1)
							combine(outputsToPrint, command.Ch)
							chCmd <- command
						} else {
							Log.Warning(err)
						}

						if opts.StopOnErr {
							select {
							case <-cancel: // already closed
							default:
								// ensure we only initiate the stop attempt once,
								// from all our command threads
								stopOnce.Do(func() {
									if opts.StopOnErr {
										Log.Error("stop on first error")
									}
									err = stopChildProcesses(opts.NoStopExes, opts.NoKillExes, opts.CleanupTime)
									if err != nil {
										if Verbose {
											Log.Error(err)
										}
										os.Exit(1)
									}
								})
							}

							stop = true
							return
						}
						if chances > 0 {
							if opts.PrintRetryOutput {
								outputsToPrint = append(outputsToPrint, ch)
							}
							if Verbose && opts.Retries > 0 {
								Log.Warningf("retry %d/%d times: %s",
									tryNumber,
									opts.Retries,
									command.Cmd)
							}
							chances--
							<-time.After(opts.RetryInterval)
							continue
						}
						return
					}
					// print final output
					outputsToPrint = append(outputsToPrint, ch)
					if opts.PropExitStatus {
						chExitStatus <- command.exitStatus
					}
					break
				}

				command.Ch = make(chan string, 1)
				combine(outputsToPrint, command.Ch)
				chCmd <- command
				if opts.RecordSuccessfulCmd {
					chSuccessfulCmd <- cmdStr
				}

			}(id, cmdStr)
			id++
		}
		wg.Wait()

		close(chCmd)
		if opts.RecordSuccessfulCmd {
			close(chSuccessfulCmd)
		}
		if opts.PropExitStatus {
			close(chExitStatus)
		}
		done <- 1
	}()
	return chCmd, chSuccessfulCmd, done, chExitStatus
}
