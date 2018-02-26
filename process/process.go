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
	"runtime"
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

	Ch                 chan string    // channel for stdout
	reader             *bufio.Reader  // reader for stdout
	tmpfile            string         // tmpfile for stdout
	tmpfh              *os.File       // file handler for tmpfile
	ImmediateDoneGroup sync.WaitGroup // wait for immediate output to be done
	finishSendOutput   bool           // a flag of whether finished sending output to Ch

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
func (c *Command) Run(opts *Options, tryNumber int, outfh *os.File, errfh *os.File) (chan string, error) {
	// create a return chan here; we will set the c.Ch in the parent
	ch := make(chan string, 1)

	if c.dryrun {
		ch <- c.Cmd + "\n"
		close(ch)
		c.finishSendOutput = true
		return ch, nil
	}

	c.Err = c.run(opts, tryNumber, outfh, errfh)

	// don't return here, keep going so we can display
	// the output from commands that error
	var readErr error = nil

	if Verbose {
		Log.Infof("finish cmd #%d in %s: %s", c.ID, c.Duration, c.Cmd)
	}

	go func() {
		if opts.ImmediateOutput {
			c.ImmediateDoneGroup.Wait()
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

var isWindows bool = runtime.GOOS == "windows"

func getShell() string {
	var shell string
	if isWindows {
		shell = os.Getenv("COMSPEC")
		if shell == "" {
			shell = "C:\\WINDOWS\\System32\\cmd.exe"
		}
	} else {
		shell = os.Getenv("SHELL")
		if shell == "" {
			shell = "sh"
		}
	}
	return shell
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

func isProcessRunning(pid int) bool {
	_, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return true
}

// ensure Windows processes go away
func killWindowsProcessTreeRecursive(childProcess *psutil.Process) {
	grandChildren, err := childProcess.Children()
	if grandChildren != nil && err == nil {
		for _, value := range grandChildren {
			killWindowsProcessTreeRecursive(value)
		}
	}
	attempts := 1
	for {
		if Verbose {
			Log.Infof("taskkill /t /f /pid %s", strconv.Itoa(int(childProcess.Pid)))
		}
		out, err := exec.Command("taskkill", "/t", "/f", "/pid", strconv.Itoa(int(childProcess.Pid))).Output()
		if Verbose {
			if err != nil {
				Log.Error(err)
			}
			Log.Infof("%s", out)
		}

		if !isProcessRunning(int(childProcess.Pid)) {
			break
		} else {
			time.Sleep(10 * time.Millisecond)
			attempts += 1
			if attempts > 30 {
				break
			}
		}
	}
}

// SafeCounter is safe to use concurrently.
type SafeCounter struct {
	v    uint64
	lock sync.Mutex
}

func (c *SafeCounter) readThenInc() uint64 {
	c.lock.Lock()
	value := c.v
	c.v++
	c.lock.Unlock()
	return value
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

func writeImmediateLines(numJobs int, cmdId uint64, tryNumber int, lineNumber *SafeCounter, reader *bufio.Reader, fh *os.File) error {
	var err error = nil
	for {
		// read line by line
		var line string
		if reader != nil {
			line, err = reader.ReadString('\n')
			// only write non-empty lines
			if len(line) > 0 && line != "\n" && line != "\r\n" {
				// we only need to prefix output if running in parallel
				if numJobs > 1 {
					// we prefix lines so that the user can differentiate the contents of the interleaved output
					// use parentheses, encoded integers, and a colon, so that the user can afterward sort the output lines
					// and get the output correctly grouped by command id, try number, and line
					lineNumberValue := lineNumber.readThenInc()
					prefix := fmt.Sprintf("(%s", lexEncode(cmdId, TopLevel))
					prefix += fmt.Sprintf("%s%s", getEntrySeparator(), lexEncode(uint64(tryNumber), TopLevel))
					prefix += fmt.Sprintf("%s%s): ", getEntrySeparator(), lexEncode(lineNumberValue, TopLevel))
					line = prefix + line
				}
				fh.WriteString(line)
			}
		} else {
			err = io.EOF
		}
		if err != nil {
			break
		}
	}
	return err
}

// run a command and pass output to c.reader.
// Note that output returns only after finishing run.
// This function is mainly borrowed from https://github.com/brentp/gargs .
func (c *Command) run(opts *Options, tryNumber int, outfh *os.File, errfh *os.File) error {
	t := time.Now()
	chCancelMonitor := make(chan struct{})
	defer func() {
		close(chCancelMonitor)
		c.Duration = time.Now().Sub(t)
	}()

	var command *exec.Cmd
	qcmd := fmt.Sprintf(`%s`, c.Cmd)
	if Verbose {
		Log.Infof("start  cmd #%d: %s", c.ID, qcmd)
	}

	if c.Timeout > 0 {
		c.ctx, c.ctxCancel = context.WithTimeout(context.Background(), c.Timeout)
		if isWindows {
			command = exec.CommandContext(c.ctx, getShell())
			c.setWindowsCommandAttr(command, qcmd)
		} else {
			command = exec.CommandContext(c.ctx, getShell(), "-c", qcmd)
		}
	} else {
		if isWindows {
			command = exec.Command(getShell())
			c.setWindowsCommandAttr(command, qcmd)
		} else {
			command = exec.Command(getShell(), "-c", qcmd)
		}
	}

	pipeStdout, err := command.StdoutPipe()
	if err != nil {
		return errors.Wrapf(err, "get stdout pipe of cmd #%d: %s", c.ID, c.Cmd)
	}
	defer pipeStdout.Close()

	var pipeStderr io.ReadCloser = nil
	if opts.ImmediateOutput {
		pipeStderr, err = command.StderrPipe()
		if err != nil {
			return errors.Wrapf(err, "get stderr pipe of cmd #%d: %s", c.ID, c.Cmd)
		}
		defer pipeStderr.Close()
	} else {
		// no code yet for stderr handling, so just have it go to os.Stderr
		command.Stderr = os.Stderr
	}

	err = command.Start()
	if err != nil {
		return errors.Wrapf(err, "start cmd #%d: %s", c.ID, c.Cmd)
	}

	var outPipe *bufio.Reader = nil
	var errPipe *bufio.Reader = nil
	if opts.ImmediateOutput {
		// use default size reader here, since reading line by line
		outPipe = bufio.NewReader(pipeStdout)
		errPipe = bufio.NewReader(pipeStderr)
	} else {
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
			if opts.KillOnCtrlC {
				if isWindows {
					childProcess, err := psutil.NewProcess(int32(command.Process.Pid))
					if err != nil {
						Log.Error(err)
					}
					killWindowsProcessTreeRecursive(childProcess)
				} else {
					command.Process.Kill()
				}
			}
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

	lineNumber := SafeCounter{v: 1}
	if opts.ImmediateOutput {
		// handle stdout
		c.ImmediateDoneGroup.Add(2) // 2 since 1 for stdout and 1 for stderr
		go func() {
			defer c.ImmediateDoneGroup.Done()
			writeImmediateLines(opts.Jobs, c.ID, tryNumber, &lineNumber, outPipe, outfh)
		}()
		// handle stderr
		go func() {
			defer c.ImmediateDoneGroup.Done()
			writeImmediateLines(opts.Jobs, c.ID, tryNumber, &lineNumber, errPipe, errfh)
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
	ImmediateOutput     bool          // print output immediately and interleaved
	PrintRetryOutput    bool          // print output from retries
	Timeout             time.Duration // timeout
	StopOnErr           bool          // stop on any error
	PropExitStatus      bool          // propagate child exit status
	KillOnCtrlC         bool          // kill child processes on ctrl-c
	RecordSuccessfulCmd bool          // send successful command to channel
	Verbose             bool
}

// Run4Output runs commands in parallel from channel chCmdStr,
// and returns an output text channel,
// and a done channel to ensure safe exit.
func Run4Output(opts *Options, cancel chan struct{}, chCmdStr chan string, outfh *os.File, errfh *os.File) (chan string, chan string, chan int, chan int) {
	if opts.Verbose {
		Verbose = true
	}
	chCmd, chSuccessfulCmd, doneChCmd, chExitStatus := Run(opts, cancel, chCmdStr, outfh, errfh)
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
					if Verbose {
						Log.Debugf("cancel receiving finished cmd")
					}
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
func Run(opts *Options, cancel chan struct{}, chCmdStr chan string, outfh *os.File, errfh *os.File) (chan *Command, chan string, chan int, chan int) {
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
		chExitStatus = make(chan int)
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
					ch, err := command.Run(opts, tryNumber, outfh, errfh)
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
								Log.Error("stop on first error(s)")
								close(cancel)
								close(chCmd)
								if opts.RecordSuccessfulCmd {
									close(chSuccessfulCmd)
								}
								if opts.PropExitStatus {
									close(chExitStatus)
								}
								done <- 1
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
		if !stop {
			close(chCmd)
			if opts.RecordSuccessfulCmd {
				close(chSuccessfulCmd)
			}
		}

		if opts.PropExitStatus {
			close(chExitStatus)
		}
		done <- 1
	}()
	return chCmd, chSuccessfulCmd, done, chExitStatus
}
