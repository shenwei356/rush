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
	"fmt"
	"os"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cznic/sortutil"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
)

// RootCmd represents the base command when called without any subcommands
var RootCmd = &cobra.Command{
	Use:   "rush",
	Short: "parallelly execute shell commands",
	Long: fmt.Sprintf(`
rush -- parallelly execute shell commands

Version: %s

Author: Wei Shen <shenwei356@gmail.com>

Documents  : http://bioinf.shenwei.me/rush
Source code: https://github.com/shenwei356/rush

`, VERSION),
	Run: func(cmd *cobra.Command, args []string) {
		var err error
		config := getConfigs(cmd)

		if config.Version {
			checkVersion()
			return
		}

		j := config.Ncpus + 1
		if j > runtime.NumCPU() {
			j = runtime.NumCPU()
		}
		runtime.GOMAXPROCS(j)

		DataBuffer = config.BufferSize
		ChanBuffer = config.Ncpus
		Verbose = config.Verbose

		config.reFieldDelimiter, err = regexp.Compile(config.FieldDelimiter)
		checkError(errors.Wrap(err, "compile field delimiter"))

		command0 := strings.Join(args, " ")
		if command0 == "" {
			command0 = "echo {}"
		}

		// out file handler
		var outfh *bufio.Writer
		if isStdin(config.OutFile) {
			outfh = bufio.NewWriter(os.Stdout)
		} else {
			var fh *os.File
			fh, err = os.Create(config.OutFile)
			defer fh.Close()

			outfh = bufio.NewWriter(fh)
			checkError(err)
		}
		defer outfh.Flush()

		if len(config.Infiles) == 0 {
			config.Infiles = append(config.Infiles, "-")
		}

		split := func(data []byte, atEOF bool) (advance int, token []byte, err error) {
			if atEOF && len(data) == 0 {
				return 0, nil, nil
			}
			i := bytes.IndexAny(data, config.RecordDelimiter)
			if i >= 0 {
				return i + 1, data[0:i], nil // trim config.RecordDelimiter
				// return i + 1, data[0 : i+1], nil
			}
			if atEOF {
				return len(data), data, nil
			}
			return 0, nil, nil
		}

		for _, file := range config.Infiles {
			// input file handler
			var infh *os.File
			if isStdin(file) {
				infh = os.Stdin
			} else {
				infh, err = os.Open(file)
				checkError(err)
				defer infh.Close()
			}

			// channel of input data
			chIn := make(chan Chunk, config.Ncpus)

			// ---------------------------------------------------------------
			// output
			// channel of Command
			chCmd := make(chan *Command, config.Ncpus)
			chOut := make(chan string, config.Ncpus)

			var wgOut sync.WaitGroup
			cancelOut := make(chan struct{})
			doneOut := make(chan int)

			doneWholeOut := make(chan int)
			go func() {
				// read from chOut and print
				go func() {
					last := time.Now().Add(2 * time.Second)
					for c := range chOut {
						outfh.WriteString(c)

						if t := time.Now(); t.After(last) {
							outfh.Flush()
							last = t.Add(2 * time.Second)
						}
					}
					outfh.Flush()

					doneOut <- 1
				}()

				if !config.KeepOrder { // do not keep order
					// fan in
					tokens := make(chan int, config.Ncpus)
					var line string
					for c := range chCmd {
						wgOut.Add(1)
						tokens <- 1
						go func(cancel chan struct{}, c *Command) {
							defer func() {
								wgOut.Done()
								<-tokens
							}()

							// output the command name
							if config.DryRun {
								chOut <- c.Cmd + "\n"
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
								if c.Done {
									// do not forget left data
									for line = range c.Ch {
										chOut <- line
									}
									checkError(errors.Wrapf(c.Cleanup(), "remove tmpfile for cmd: %s", c.Cmd))
									break LOOP
								}
							}
						}(cancelOut, c)
					}
				} else {
					wgOut.Add(1)

					var id uint64 = 1
					var c, c1 *Command
					var ok bool
					var line string
					cmds := make(map[uint64]*Command)
					for c = range chCmd {
						if c.ID == id { // your turn
							if config.DryRun {
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
									if config.DryRun {
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
							if config.DryRun {
								chOut <- c.Cmd + "\n"
							} else {
								for line = range c.Ch {
									chOut <- line
								}
								checkError(errors.Wrapf(c.Cleanup(), "remove tmpfile for cmd: %s", c.Cmd))
							}
						}
					}

					wgOut.Done()
				}

				doneWholeOut <- 1
			}()

			// ---------------------------------------------------------------
			// producer
			go func() {
				defer close(chIn)
				scanner := bufio.NewScanner(infh)
				scanner.Buffer(make([]byte, 0, 16384), 2147483648)
				scanner.Split(split)

				n := config.NRecords
				var id uint64 = 1

				var records []string
				records = make([]string, 0, n)
				for scanner.Scan() {
					records = append(records, scanner.Text())

					if len(records) == n {
						chIn <- Chunk{ID: id, Data: records}
						id++
						records = make([]string, 0, n)
					}
				}
				if len(records) > 0 {
					chIn <- Chunk{ID: id, Data: records}
					id++
				}

				checkError(errors.Wrap(scanner.Err(), "read input data"))
			}()

			// ---------------------------------------------------------------
			// consumer
			var wgWorkers sync.WaitGroup
			tokens := make(chan int, config.Ncpus)
			for c := range chIn {
				wgWorkers.Add(1)
				tokens <- 1
				go func(c Chunk) {
					defer func() {
						wgWorkers.Done()
						<-tokens
					}()

					command := NewCommand(c.ID,
						fillCommand(config, command0, c),
						time.Duration(config.Timeout)*time.Second)

					// dry run
					if config.DryRun {
						chCmd <- command
						return
					}

					err = command.Run()
					checkError(err)
					chCmd <- command
				}(c)
			}

			// the order is very important!
			wgWorkers.Wait() // wait workers done

			close(chCmd) // close Cmd channel
			wgOut.Wait() //
			close(chOut) // close output chanel

			<-doneOut      // finish output
			<-doneWholeOut // wait output done
		}
	},
}

// Chunk is []string with ID
type Chunk struct {
	ID   uint64
	Data []string
}

// ChunkCommand is
type ChunkCommand struct {
	ID  uint64
	Cmd *Command
}

// Execute adds all child commands to the root command sets flags appropriately.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute() {
	if err := RootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(-1)
	}
}

func init() {
	RootCmd.Flags().BoolP("verbose", "v", false, "print verbose information")
	RootCmd.Flags().BoolP("version", "V", false, `print version information and check for update`)

	RootCmd.Flags().IntP("ncpus", "j", runtime.NumCPU(), "run n jobs in parallel")
	RootCmd.Flags().StringP("out-file", "o", "-", `out file ("-" for stdout)`)

	RootCmd.Flags().StringSliceP("infile", "i", []string{}, "input data file")

	RootCmd.Flags().StringP("record-delimiter", "D", "\n", "record delimiter")
	RootCmd.Flags().IntP("nrecords", "n", 1, "number of records sent to a command")
	RootCmd.Flags().StringP("field-delimiter", "d", `\s+`, "field delimiter in records")

	RootCmd.Flags().IntP("retries", "r", 0, "maximum retries")
	RootCmd.Flags().IntP("retry-interval", "", 0, "retry interval (unit: second)")
	RootCmd.Flags().IntP("timeout", "t", 0, "timeout of a command (unit: second, 0 for no timeout)")

	RootCmd.Flags().BoolP("keep-order", "k", false, "keep output in order of input")
	RootCmd.Flags().BoolP("stop-on-error", "e", false, "stop all processes on any error")
	RootCmd.Flags().BoolP("continue", "c", false, `continue run commands except for finished commands in "finished.txt"`)
	RootCmd.Flags().BoolP("dry-run", "", false, "print command but not run")

	RootCmd.Flags().IntP("buffer-size", "", 1, "buffer size for output of command before saving to tmpfile (unit: Mb)")
}

// Config is the struct containing all global flags
type Config struct {
	Verbose bool
	Version bool

	Ncpus   int
	OutFile string

	Infiles []string

	RecordDelimiter  string
	NRecords         int
	FieldDelimiter   string
	reFieldDelimiter *regexp.Regexp

	Retries       int
	RetryInterval int
	Timeout       int

	KeepOrder bool
	StopOnErr bool
	Continue  bool
	DryRun    bool

	BufferSize int
}

func getConfigs(cmd *cobra.Command) Config {
	return Config{
		Verbose: getFlagBool(cmd, "verbose"),
		Version: getFlagBool(cmd, "version"),

		Ncpus:   getFlagPositiveInt(cmd, "ncpus"),
		OutFile: getFlagString(cmd, "out-file"),

		Infiles: getFlagStringSlice(cmd, "infile"),

		RecordDelimiter: getFlagString(cmd, "record-delimiter"),
		NRecords:        getFlagPositiveInt(cmd, "nrecords"),
		FieldDelimiter:  getFlagString(cmd, "field-delimiter"),

		Retries:       getFlagNonNegativeInt(cmd, "retries"),
		RetryInterval: getFlagNonNegativeInt(cmd, "retry-interval"),
		Timeout:       getFlagNonNegativeInt(cmd, "timeout"),

		KeepOrder: getFlagBool(cmd, "keep-order"),
		StopOnErr: getFlagBool(cmd, "stop-on-error"),
		Continue:  getFlagBool(cmd, "continue"),
		DryRun:    getFlagBool(cmd, "dry-run"),

		BufferSize: getFlagPositiveInt(cmd, "buffer-size") * 1048576,
	}
}
