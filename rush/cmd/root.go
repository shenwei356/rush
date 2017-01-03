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
	"runtime"
	"strings"

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
		config := getConfigs(cmd)

		if getFlagBool(cmd, "version") {
			checkVersion()
			return
		}

		var err error
		var outfh *os.File
		if isStdin(config.OutFile) {
			outfh = os.Stdout
		} else {
			outfh, err = os.Create(config.OutFile)
			defer outfh.Close()
			checkError(err)
		}

		infiles := getFlagStringSlice(cmd, "infile")
		if len(infiles) == 0 {
			infiles = append(infiles, "-")
		}

		split := func(data []byte, atEOF bool) (advance int, token []byte, err error) {
			if atEOF && len(data) == 0 {
				return 0, nil, nil
			}
			i := bytes.IndexAny(data, config.RecordDelimiter)
			if i >= 0 {
				return i + 1, data[0 : i+1], nil
			}
			if atEOF {
				return len(data), data, nil
			}
			return 0, nil, nil
		}

		for _, file := range infiles {
			var infh *os.File
			if isStdin(file) {
				infh = os.Stdin
			} else {
				infh, err = os.Open(file)
				checkError(err)
				defer infh.Close()
			}

			scanner := bufio.NewScanner(infh)
			scanner.Buffer(make([]byte, 0, 16384), 2147483648)
			scanner.Split(split)

			// go func() {
			var records []string
			records = make([]string, 0, config.NRecords)
			for scanner.Scan() {
				records = append(records, scanner.Text())
				if len(records) == config.NRecords {
					outfh.WriteString(fmt.Sprintf("{\n%s}\n\n", strings.Join(records, "")))
					records = make([]string, 0, config.NRecords)
				}
			}
			if len(records) > 0 {
				outfh.WriteString(fmt.Sprintf("{\n%s}\n\n", strings.Join(records, "")))
			}
			checkError(scanner.Err())
			// }()
		}

	},
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

	RootCmd.Flags().IntP("ncpus", "j", runtime.NumCPU(), "number of CPUs")
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

	RecordDelimiter string
	NRecords        int
	FieldDelimiter  string

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
