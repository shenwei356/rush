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
	"strings"
	"syscall"
	"time"

	"github.com/pkg/errors"
)

// Command is
type Command struct {
	ID  uint64
	Cmd string

	Timeout time.Duration
	ctx     context.Context
	cancel  context.CancelFunc

	Ch   chan string // buffer for output
	Done bool

	Err      error
	Duration time.Duration

	reader  *bufio.Reader
	tmpfile string
	tmpfh   *os.File
}

// NewCommand create a Command
func NewCommand(id uint64, cmdStr string, timeout time.Duration) *Command {
	command := &Command{ID: id, Cmd: strings.TrimLeft(cmdStr, " "), Timeout: timeout}
	return command
}

var tmpfilePrefix = fmt.Sprintf("rush.%d.", os.Getpid())

// DataBuffer is buffer size for output of command before saving to tmpfile
var DataBuffer = 1048576

// ChanBuffer is buffer size of output channel
var ChanBuffer = runtime.NumCPU()

// Run starts to run
func (c *Command) Run() error {
	c.Ch = make(chan string, ChanBuffer*runtime.NumCPU())
	// start command
	go func() {
		c.Err = c.run()
		if c.Err != nil {
			return
		}

		if c.tmpfile != "" { // data saved in tempfile
			// fmt.Printf("read tmpfile data from: %s\n", c.Cmd)
			c.tmpfh, c.Err = os.Open(c.tmpfile)
			defer c.tmpfh.Close()
			c.reader = bufio.NewReader(c.tmpfh)
		} else {
			// fmt.Printf("read live data from: %s\n", c.Cmd)
		}

		var line string
		for {
			line, c.Err = c.reader.ReadString('\n')
			// fmt.Printf("read data from: %s: %s, %s\n", c.Cmd, line, c.Err)
			if c.Err != nil {
				if c.Err == io.EOF {
					c.Ch <- line
					c.Err = nil
				}
				break
			}
			c.Ch <- line
		}
		// fmt.Printf("finished read data from: %s\n", c.Cmd)
		close(c.Ch)
		c.Done = true
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
	if c.tmpfile != "" {
		// fmt.Printf("remove tmpfile: %s for cmd: %s\n", c.tmpfile, c.Cmd)
		return os.Remove(c.tmpfile)
	}
	return nil
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

func (c *Command) run() error {
	var command *exec.Cmd
	qcmd := fmt.Sprintf(`%s`, c.Cmd)
	if c.Timeout > 0 {
		c.ctx, c.cancel = context.WithTimeout(context.Background(), c.Timeout)
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

	// fmt.Printf("create tmpfile for command: %s\n", c.Cmd)

	// more than DataBuffer bytes in output. must use tmpfile
	if err != nil {
		return errors.Wrapf(err, "run command: %s", c.Cmd)
	}

	c.tmpfh, err = ioutil.TempFile("", tmpfilePrefix)
	if err != nil {
		return errors.Wrapf(err, "create tmpfile for command: %s", c.Cmd)
	}
	defer c.tmpfh.Close()

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
	err = command.Wait()
	if err != nil {
		return errors.Wrapf(err, "wait command: %s", c.Cmd)
	}

	return nil
}
