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
	"strings"
	"time"

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

		j := config.Jobs + 1
		if j > runtime.NumCPU() {
			j = runtime.NumCPU()
		}
		runtime.GOMAXPROCS(j)

		config.reFieldDelimiter, err = regexp.Compile(config.FieldDelimiter)
		checkError(errors.Wrap(err, "compile field delimiter"))

		command0 := strings.Join(args, " ")
		if command0 == "" {
			command0 = "echo {}"
		}

		// -----------------------------------------------------------------

		cancel := make(chan struct{})
		TmpOutputDataBuffer = config.BufferSize
		opts := &Options{
			DryRun:    config.DryRun,
			Jobs:      config.Jobs,
			KeepOrder: config.KeepOrder,
			Retries:   config.Retries,
			Timeout:   time.Duration(config.Timeout) * time.Second,
			StopOnErr: config.StopOnErr,
			Verbose:   config.Verbose,
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

		// split function for scanner
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

			// ---------------------------------------------------------------

			// channel of command
			chCmdStr := make(chan string, config.Jobs)

			// ---------------------------------------------------------------

			// read data and generate command
			donePreprocess := make(chan int)
			go func() {
				scanner := bufio.NewScanner(infh)
				scanner.Buffer(make([]byte, 0, 16384), 2147483647)
				scanner.Split(split)

				n := config.NRecords
				var id uint64 = 1

				var records []string
				records = make([]string, 0, n)
				for scanner.Scan() {
					records = append(records, scanner.Text())

					if len(records) == n {
						chCmdStr <- fillCommand(config, command0, Chunk{ID: id, Data: records})
						id++
						records = make([]string, 0, n)
					}
				}
				if len(records) > 0 {
					chCmdStr <- fillCommand(config, command0, Chunk{ID: id, Data: records})
					id++
				}

				checkError(errors.Wrap(scanner.Err(), "read input data"))

				close(chCmdStr)

				if Verbose {
					log.Infof("finished reading input data")
				}
				donePreprocess <- 1
			}()

			// ---------------------------------------------------------------

			// output
			chOutput, doneSendOutput := Run4Output(opts, cancel, chCmdStr)

			// read from chOutput and print
			doneOutput := make(chan int)
			go func() {
				last := time.Now().Add(2 * time.Second)
				for c := range chOutput {
					outfh.WriteString(c)

					if t := time.Now(); t.After(last) {
						outfh.Flush()
						last = t.Add(2 * time.Second)
					}
				}
				outfh.Flush()

				doneOutput <- 1
			}()

			// ---------------------------------------------------------------

			// the order is very important!
			<-donePreprocess // finish read data and send command
			<-doneSendOutput // finish send output
			<-doneOutput     // finish print output
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
	RootCmd.Flags().BoolP("verbose", "", false, "print verbose information")
	RootCmd.Flags().BoolP("version", "V", false, `print version information and check for update`)

	RootCmd.Flags().IntP("jobs", "j", runtime.NumCPU(), "run n jobs in parallel")
	RootCmd.Flags().StringP("out-file", "o", "-", `out file ("-" for stdout)`)

	RootCmd.Flags().StringSliceP("infile", "i", []string{}, "input data file")

	RootCmd.Flags().StringP("record-delimiter", "D", "\n", "record delimiter")
	RootCmd.Flags().IntP("nrecords", "n", 1, "number of records sent to a command")
	RootCmd.Flags().StringP("field-delimiter", "d", `\s+`, "field delimiter in records")

	RootCmd.Flags().IntP("retries", "r", 0, "maximum retries")
	RootCmd.Flags().IntP("retry-interval", "", 0, "retry interval (unit: second)")
	RootCmd.Flags().IntP("timeout", "t", 0, "timeout of a command (unit: second, 0 for no timeout)")

	RootCmd.Flags().BoolP("keep-order", "k", false, "keep output in order of input")
	RootCmd.Flags().BoolP("stop-on-error", "e", false, "stop all processes on first error")
	RootCmd.Flags().BoolP("continue", "c", false, `continue run commands except for finished commands in "finished.txt"`)
	RootCmd.Flags().BoolP("dry-run", "", false, "print command but not run")

	RootCmd.Flags().IntP("buffer-size", "", 1, "buffer size for output of a command before saving to tmpfile (unit: Mb)")

	RootCmd.Flags().StringSliceP("assign", "v", []string{}, "assign the value val to the variable var (format var=val)")
	RootCmd.Flags().StringP("trim", "", "", `trim white space in input (available values: "l" for left, "r" for right, "lr", "rl", "b" for both side)`)
}

// Config is the struct containing all global flags
type Config struct {
	Verbose bool
	Version bool

	Jobs    int
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

	AssignMap map[string]string
	Trim      string
}

// var=value
var reAssign = regexp.MustCompile(`\s*([^=]+)\s*=(.+)`)

func getConfigs(cmd *cobra.Command) Config {
	trim := getFlagString(cmd, "trim")
	if trim != "" {
		trim = strings.ToLower(trim)
		switch trim {
		case "l":
		case "r":
		case "lr", "rl", "b":
		default:
			checkError(fmt.Errorf(`illegal value for flag --trim: %s. (available values: "l" for left, "r" for right, "lr", "rl", "b" for both side)`, trim))
		}
	}

	assignStrs := getFlagStringSlice(cmd, "assign")
	assignMap := make(map[string]string)
	for _, s := range assignStrs {
		found := reAssign.FindStringSubmatch(s)
		assignMap[found[1]] = found[2]
	}

	return Config{
		Verbose: getFlagBool(cmd, "verbose"),
		Version: getFlagBool(cmd, "version"),

		Jobs:    getFlagPositiveInt(cmd, "jobs"),
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

		Trim:      trim,
		AssignMap: assignMap,
	}
}
