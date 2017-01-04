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

		j := config.Ncpus
		if j > runtime.NumCPU() {
			j = runtime.NumCPU()
		}
		runtime.GOMAXPROCS(j)

		config.reFieldDelimiter, err = regexp.Compile(config.FieldDelimiter)
		checkError(errors.Wrap(err, "compile field delimiter"))

		command := strings.Join(args, " ")
		if command == "" {
			command = "echo {}"
		}

		if config.Version {
			checkVersion()
			return
		}

		var outfh *os.File
		if isStdin(config.OutFile) {
			outfh = os.Stdout
		} else {
			outfh, err = os.Create(config.OutFile)
			defer outfh.Close()
			checkError(err)
		}

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
			// input file handle
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
			// channel of output data
			chOut := make(chan Chunk, config.Ncpus)
			doneChOut := make(chan int)
			go func() {
				var line string
				if !config.KeepOrder { // do not keep order
					for c := range chOut {
						for _, line = range c.Data {
							outfh.WriteString(line)
						}
					}
				} else {
					var id uint64 = 1
					var c, c1 Chunk
					var ok bool
					chunks := make(map[uint64]Chunk)
					for c = range chOut {
						if c.ID == id { // your turn
							for _, line = range c.Data {
								outfh.WriteString(line)
							}

							id++
						} else { // wait the ID come out
							for true {
								if c1, ok = chunks[id]; ok {
									for _, line = range c1.Data {
										outfh.WriteString(line)
									}

									delete(chunks, c1.ID)
									id++
								} else {
									break
								}
							}
							chunks[c.ID] = c
						}
					}
					if len(chunks) > 0 {
						ids := make(sortutil.Uint64Slice, len(chunks))
						i := 0
						for id = range chunks {
							ids[i] = id
							i++
						}
						sort.Sort(ids)
						for _, id = range ids {
							c := chunks[id]
							for _, line = range c.Data {
								outfh.WriteString(line)
							}
						}
					}
				}
				doneChOut <- 1
			}()

			// ---------------------------------------------------------------
			// producer
			go func() {
				defer close(chIn)
				scanner := bufio.NewScanner(infh)
				scanner.Buffer(make([]byte, 0, 16384), 2147483648)
				scanner.Split(split)

				n := config.NRecords
				// lenRD := len(config.RecordDelimiter)
				var id uint64 = 1

				var records []string
				records = make([]string, 0, n)
				for scanner.Scan() {
					records = append(records, scanner.Text())

					if len(records) == n {
						// remove last config.RecordDelimiter
						// records[len(records)-1] = records[len(records)-1][0 : len(records[len(records)-1])-lenRD]
						chIn <- Chunk{ID: id, Data: records}
						id++
						records = make([]string, 0, n)
					}
				}
				if len(records) > 0 {
					// remove last config.RecordDelimiter
					// records[len(records)-1] = records[len(records)-1][0 : len(records[len(records)-1])-lenRD]
					chIn <- Chunk{ID: id, Data: records}
					id++
				}

				checkError(errors.Wrap(scanner.Err(), "read input data"))
			}()

			// ---------------------------------------------------------------
			// consumer
			var wg sync.WaitGroup
			tokens := make(chan int, config.Ncpus)

			for c := range chIn {
				wg.Add(1)
				tokens <- 1
				go func(c Chunk) {
					defer func() {
						wg.Done()
						<-tokens
					}()

					// worker
					chOut <- Chunk{
						ID:   c.ID,
						Data: []string{fillCommand(config, command, c) + "\n"},
					}

				}(c)
			}

			wg.Wait()    // wait workers done
			close(chOut) // close output chanel
			<-doneChOut  // wait output done
		}
	},
}

// Chunk is []string with ID
type Chunk struct {
	ID   uint64
	Data []string
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
	}
}
