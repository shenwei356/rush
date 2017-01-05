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

package cmd

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
	"github.com/pkg/errors"
)

// Command is
type Command struct {
	ID  uint64
	Cmd string

	Cancel    chan struct{}
	Timeout   time.Duration
	ctx       context.Context
	ctxCancel context.CancelFunc

	Ch               chan string // buffer for output
	finishSendOutput bool

	Err      error
	Duration time.Duration

	reader  *bufio.Reader
	tmpfile string
	tmpfh   *os.File
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

// Verbose prints extra information
var Verbose bool

var tmpfilePrefix = fmt.Sprintf("rush.%d.", os.Getpid())

// DataBuffer is buffer size for output of command before saving to tmpfile
var DataBuffer = 1048576

// OutputBufferSize is
var OutputBufferSize = 16384 // 16K

// Run starts to run
func (c *Command) Run() error {
	c.Ch = make(chan string, runtime.NumCPU())

	c.Err = c.run()
	if c.Err != nil {
		return c.Err
	}

	go func() {
		if c.tmpfile != "" { // data saved in tempfile
			c.reader = bufio.NewReader(c.tmpfh)
		}

		buf := make([]byte, OutputBufferSize)
		var n int
		for {
			n, c.Err = c.reader.Read(buf)
			if c.Err != nil {
				if c.Err == io.EOF || n < OutputBufferSize {
					c.Ch <- string(buf[0:n])
					c.Err = nil
				}
				break
			}
			c.Ch <- string(buf[0:n])
		}

		if Verbose {
			log.Infof("finished reading data from: %s", c.Cmd)
		}

		close(c.Ch)
		c.finishSendOutput = true
	}()
	return nil
}

func getShell() string {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "sh"
	}
	return shell
}

// Cleanup remove tmpfile
func (c *Command) Cleanup() error {
	var err error
	if c.tmpfh != nil {
		if Verbose {
			log.Infof("close tmpfh for: %s", c.Cmd)
		}
		err = c.tmpfh.Close()
		if err != nil {
			return err
		}
	}

	if c.tmpfile != "" {
		if Verbose {
			log.Infof("remove tmpfile of command: %s", c.Cmd)
		}
		err = os.Remove(c.tmpfile)
	}
	return err
}

// ExitCode returns exit code
func (c *Command) ExitCode() int {
	if c.Err == nil {
		return 0
	}
	if ex, ok := c.Err.(*exec.ExitError); ok {
		if st, ok := ex.Sys().(syscall.WaitStatus); ok {
			return st.ExitStatus()
		}
	}
	return -1
}

// run a command, note that output returns only after finishing run.
func (c *Command) run() error {
	var command *exec.Cmd
	qcmd := fmt.Sprintf(`%s`, c.Cmd)
	if Verbose {
		log.Infof("run command: %s", qcmd)
	}

	if c.Timeout > 0 {
		c.ctx, c.ctxCancel = context.WithTimeout(context.Background(), c.Timeout)
		command = exec.CommandContext(c.ctx, getShell(), "-c", qcmd)
	} else {
		command = exec.Command(getShell(), "-c", qcmd)
	}

	pipeStdout, err := command.StdoutPipe()
	if err != nil {
		return errors.Wrapf(err, "get stdout pipe of command: %s", c.Cmd)
	}
	defer pipeStdout.Close()

	command.Stderr = os.Stderr

	err = command.Start()
	if err != nil {
		checkError(errors.Wrapf(err, "start command: %s", c.Cmd))
	}

	bpipe := bufio.NewReaderSize(pipeStdout, DataBuffer)

	var readed []byte
	readed, err = bpipe.Peek(DataBuffer)

	// less than DataBuffer bytes in output...
	if err == bufio.ErrBufferFull || err == io.EOF {
		err = command.Wait()
		if err != nil {
			return errors.Wrapf(err, "wait command: %s", c.Cmd)
		}
		c.reader = bufio.NewReader(bytes.NewReader(readed))
		return nil
	}

	if Verbose {
		log.Infof("create tmpfile for command: %s", c.Cmd)
	}

	// more than DataBuffer bytes in output. must use tmpfile
	if err != nil {
		return errors.Wrapf(err, "run command: %s", c.Cmd)
	}

	c.tmpfh, err = ioutil.TempFile("", tmpfilePrefix)
	if err != nil {
		return errors.Wrapf(err, "create tmpfile for command: %s", c.Cmd)
	}
	// defer c.tmpfh.Close()

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
		err = command.Wait()
	}
	if err != nil {
		return errors.Wrapf(err, "wait command: %s", c.Cmd)
	}

	if Verbose {
		log.Infof("finished command: %s", c.Cmd)
	}

	return nil
}

// Options is
type Options struct {
	DryRun    bool
	Workers   int
	KeepOrder bool
	Retries   int
	Timeout   time.Duration
	Verbose   bool
}

// Run4Output is
func Run4Output(opts *Options, cancel chan struct{}, chCmdStr chan string) (chan string, chan int) {
	if opts.Verbose {
		Verbose = true
	}
	chCmd, doneChCmd := Run(opts, cancel, chCmdStr)
	chOut := make(chan string, opts.Workers)
	done := make(chan int)

	go func() {
		var wg sync.WaitGroup
		if !opts.KeepOrder { // do not keep order
			tokens := make(chan int, opts.Workers)
			var line string

			for c := range chCmd {
				wg.Add(1)
				tokens <- 1

				go func(c *Command) {
					defer func() {
						wg.Done()
						<-tokens
					}()

					// output the command name
					if opts.DryRun {
						chOut <- c.Cmd + "\n"
						if Verbose {
							log.Infof("finished sending cmd name: %s", c.Cmd)
						}
						return
					}

					// read data from channel and outpput
				LOOP:
					for {
						select {
						case chOut <- <-c.Ch:
						case <-cancel:
							break LOOP
						}
						if c.finishSendOutput {
							// do not forget the left data
							for line = range c.Ch {
								chOut <- line
							}
							checkError(errors.Wrapf(c.Cleanup(), "remove tmpfile for cmd: %s", c.Cmd))
							break LOOP
						}
					}

					if Verbose {
						log.Infof("finished reseiving data from: %s", c.Cmd)
					}
				}(c)
			}

		} else { // keep drder
			wg.Add(1)

			var id uint64 = 1
			var c, c1 *Command
			var ok bool
			var line string
			cmds := make(map[uint64]*Command)
			for c = range chCmd {
				if c.ID == id { // your turn
					if opts.DryRun {
						chOut <- c.Cmd + "\n"
					} else {
						for line = range c.Ch {
							chOut <- line
						}
						checkError(errors.Wrapf(c.Cleanup(), "remove tmpfile for cmd: %s", c.Cmd))
					}

					id++
				} else { // wait the ID come out
					for true {
						if c1, ok = cmds[id]; ok {
							if opts.DryRun {
								chOut <- c1.Cmd + "\n"
							} else {
								for line = range c1.Ch {
									chOut <- line
								}
								checkError(errors.Wrapf(c1.Cleanup(), "remove tmpfile for cmd: %s", c1.Cmd))
							}

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
					if opts.DryRun {
						chOut <- c.Cmd + "\n"
					} else {
						for line = range c.Ch {
							chOut <- line
						}
						checkError(errors.Wrapf(c.Cleanup(), "remove tmpfile for cmd: %s", c.Cmd))
					}
				}
			}

			wg.Done()
		}

		<-doneChCmd
		wg.Wait()
		close(chOut)

		if Verbose {
			log.Infof("finished sending all output")
		}
		done <- 1
	}()
	return chOut, done
}

// Run is
func Run(opts *Options, cancel chan struct{}, chCmdStr chan string) (chan *Command, chan int) {
	if opts.Verbose {
		Verbose = true
	}

	chCmd := make(chan *Command, opts.Workers)
	done := make(chan int)

	go func() {
		var wg sync.WaitGroup
		tokens := make(chan int, opts.Workers)
		var id uint64 = 1
		for cmdStr := range chCmdStr {
			wg.Add(1)
			tokens <- 1

			go func(id uint64, cmdStr string) {
				defer func() {
					wg.Done()
					<-tokens
				}()

				command := NewCommand(id, cmdStr, cancel, opts.Timeout)
				err := command.Run()
				if err != nil { // fail to run
					return
				}

				chCmd <- command
			}(id, cmdStr)
			id++
		}

		wg.Wait()
		close(chCmd)
		if Verbose {
			log.Infof("finished running all %d commands", id-1)
		}
		done <- 1
	}()

	return chCmd, done
}
