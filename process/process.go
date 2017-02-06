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
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cznic/sortutil"
	"github.com/op/go-logging"
	"github.com/pkg/errors"
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

	Ch               chan string   // channel for stdout
	reader           *bufio.Reader // reader for stdout
	tmpfile          string        // tmpfile for stdout
	tmpfh            *os.File      // file handler for tmpfile
	finishSendOutput bool          // a flag of whether finished sending output to Ch

	Err      error         // Error
	Duration time.Duration // runtime

	dryrun bool
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

// Verbose decide whether print extra information
var Verbose bool

var tmpfilePrefix = fmt.Sprintf("rush.%d.", os.Getpid())

// TmpOutputDataBuffer is buffer size for output of a command before saving to tmpfile,
// default 1M.
var TmpOutputDataBuffer = 1048576 // 1M

// OutputChunkSize is buffer size of output string chunk sent to channel, default 16K.
var OutputChunkSize = 16384 // 16K

// Run runs a command and send output to command.Ch in background.
func (c *Command) Run() error {
	c.Ch = make(chan string, 1)

	if c.dryrun {
		c.Ch <- c.Cmd + "\n"
		close(c.Ch)
		c.finishSendOutput = true
		return nil
	}

	c.Err = c.run()
	if c.Err != nil {
		return c.Err
	}

	if Verbose {
		Log.Infof("finish cmd #%d in %s: %s", c.ID, c.Duration, c.Cmd)
	}

	go func() {
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
			n, c.Err = c.reader.Read(buf)

			existedN = b.Len()
			b.Write(buf[0:n])

			if c.Err != nil {
				if c.Err == io.EOF {
					if b.Len() > 0 {
						// if Verbose {
						// 	N += uint64(b.Len())
						// }
						c.Ch <- b.String() // string(buf[0:n])
					}
					b.Reset()
					c.Err = nil
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
			c.Ch <- string(bb[0 : i+1]) // string(buf[0:n])

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

		close(c.Ch)
		c.finishSendOutput = true
	}()
	return nil
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
		// if Verbose {
		// 	Log.Infof("remove tmpfile of command: %s", c.Cmd)
		// }
		err = os.Remove(c.tmpfile)
	}
	return err
}

// ExitCode returns the exit code associated with a given error
func (c *Command) ExitCode() int {
	if c.Err == nil {
		return 0
	}
	if ex, ok := c.Err.(*exec.ExitError); ok {
		if st, ok := ex.Sys().(syscall.WaitStatus); ok {
			return st.ExitStatus()
		}
	}
	return 1
}

// ErrTimeout means command timeout
var ErrTimeout = fmt.Errorf("time out")

// ErrCancelled means command being cancelled
var ErrCancelled = fmt.Errorf("cancelled")

// run a command and pass output to c.reader.
// Note that output returns only after finishing run.
// This function is mainly borrowed from https://github.com/brentp/gargs .
func (c *Command) run() error {
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
			command = exec.CommandContext(c.ctx, getShell(), "/c", qcmd)
		} else {
			command = exec.CommandContext(c.ctx, getShell(), "-c", qcmd)
		}
	} else {
		if isWindows {
			command = exec.Command(getShell(), "/c", qcmd)
		} else {
			command = exec.Command(getShell(), "-c", qcmd)
		}
	}

	pipeStdout, err := command.StdoutPipe()
	if err != nil {
		return errors.Wrapf(err, "get stdout pipe of cmd #%d: %s", c.ID, c.Cmd)
	}
	defer pipeStdout.Close()

	command.Stderr = os.Stderr

	err = command.Start()
	if err != nil {
		return errors.Wrapf(err, "start cmd #%d: %s", c.ID, c.Cmd)
	}

	bpipe := bufio.NewReaderSize(pipeStdout, TmpOutputDataBuffer)

	chErr := make(chan error, 2) // may come from three sources, must be buffered
	chEndBeforeTimeout := make(chan struct{})

	go func() {
		select {
		case <-c.Cancel:
			if Verbose {
				Log.Warningf("cancel cmd #%d: %s", c.ID, c.Cmd)
			}
			chErr <- ErrCancelled
			command.Process.Kill()
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
			readed, err = bpipe.Peek(TmpOutputDataBuffer)
			chErr <- err
		}()
		err = <-chErr // from timeout #T or peek #P
	} else {
		readed, err = bpipe.Peek(TmpOutputDataBuffer)
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

		if err != nil {
			return errors.Wrapf(err, "wait cmd #%d: %s", c.ID, c.Cmd)
		}
		c.reader = bufio.NewReader(bytes.NewReader(readed))
		return nil
	}

	// more than TmpOutputDataBuffer bytes in output. must use tmpfile
	if err != nil {
		return errors.Wrapf(err, "run cmd #%d: %s", c.ID, c.Cmd)
	}

	// if Verbose {
	// 	Log.Infof("create tmpfile for command: %s", c.Cmd)
	// }

	c.tmpfh, err = ioutil.TempFile("", tmpfilePrefix)
	if err != nil {
		return errors.Wrapf(err, "create tmpfile for cmd #%d: %s", c.ID, c.Cmd)
	}

	c.tmpfile = c.tmpfh.Name()

	btmp := bufio.NewWriter(c.tmpfh)
	_, err = io.CopyBuffer(btmp, bpipe, readed)
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
	Timeout             time.Duration // timeout
	StopOnErr           bool          // stop on any error
	RecordSuccessfulCmd bool          // send successful command to channel
	Verbose             bool
}

// Run4Output runs commands in parallel from channel chCmdStr,
// and returns an output text channel,
// and a done channel to ensure safe exit.
func Run4Output(opts *Options, cancel chan struct{}, chCmdStr chan string) (chan string, chan string, chan int) {
	if opts.Verbose {
		Verbose = true
	}
	chCmd, chSuccessfulCmd, doneChCmd := Run(opts, cancel, chCmdStr)
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
					for true {
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
	return chOut, chSuccessfulCmd, done
}

// Run runs commands in parallel from channel chCmdStr，
// and returns a Command channel,
// and a done channel to ensure safe exit.
func Run(opts *Options, cancel chan struct{}, chCmdStr chan string) (chan *Command, chan string, chan int) {
	if opts.Verbose {
		Verbose = true
	}

	chCmd := make(chan *Command, opts.Jobs)
	var chSuccessfulCmd chan string
	if opts.RecordSuccessfulCmd {
		chSuccessfulCmd = make(chan string, opts.Jobs)
	}
	done := make(chan int)

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
				for {
					err := command.Run()
					if err != nil { // fail to run
						if chances == 0 || opts.StopOnErr {
							Log.Error(err)
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
								done <- 1
							}

							stop = true
							return
						}
						if chances > 0 {
							if Verbose && opts.Retries > 0 {
								Log.Warningf("retry %d/%d times: %s",
									opts.Retries-chances+1,
									opts.Retries, command.Cmd)
							}
							chances--
							<-time.After(opts.RetryInterval)
							continue
						}
						return
					}
					break
				}

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

		done <- 1
	}()
	return chCmd, chSuccessfulCmd, done
}
