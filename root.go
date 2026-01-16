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

package main

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/signal"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/pkg/errors"
	pb "github.com/schollz/progressbar/v3"
	"github.com/shenwei356/rush/process"
	"github.com/shenwei356/util/stringutil"
	"github.com/shenwei356/xopen"
	"github.com/spf13/cobra"
)

// RootCmd represents the base command when called without any subcommands
var RootCmd = &cobra.Command{
	Use:   "rush",
	Short: "a cross-platform command-line tool for executing jobs in parallel",
	Long: fmt.Sprintf(`rush -- a cross-platform command-line tool for executing jobs in parallel

Version: %s

Author: Wei Shen <shenwei356@gmail.com>

Homepage: https://github.com/shenwei356/rush

Input:
  - Input could be a list of strings or numbers, e.g., file paths.
  - Input can be given either from the STDIN or file(s) via the option -i/--infile.
  - Some options could be used to defined how the input records are parsed:
    -d, --field-delimiter   field delimiter in records (default "\s+")
    -D, --record-delimiter  record delimiter (default "\n")
    -n, --nrecords          number of records sent to a command (default 1)
    -J, --records-join-sep  record separator for joining multi-records (default "\n")
    -T, --trim              trim white space (" \t\r\n") in input

Output:
  - Outputs of all commands are written to STDOUT by default,
    you can also use -o/--out-file to specify a output file.
  - Outputs of all commands are random, you can use the flag -k/--keep-order
    to keep output in order of input.
  - Outputs of all commands are buffered, you can use the flag -I/--immediate-output
    to print output immediately and interleaved.

Replacement strings in commands:
  {}          full data
  {n}         nth field in delimiter-delimited data
  {/}         dirname
  {%%}         basename
  {.}         remove the last file extension
  {:}         remove all file extensions.
  {^suffix}   remove suffix
  {@regexp}   capture submatch using regular expression.
              Limitation: curly brackets can't be used in the regexp.
  {#}         job ID
  {?}         a value computed as $cpus / $jobs, which can be used as the number of
              threads for each command. This value is dynamically adjusted according
              to the number of jobs (-j/--jobs).

  Escaping curly brackets "{}":
    {{}}        {}
    {{1}}       {1}
    {{1,}}      {1,}
    {{a}}       {a}

  Combinations:
    {%%.}, {%%:}            basename without extension
    {2.}, {2/}, {2%%.}     manipulate nth field
    {file:}, {file:^_1}   remove all extensions of a preset variable (see below)

Preset variable (macro):
  1. You can pass variables to the command like awk via the option -v. E.g.,
     $ seq 3 | rush -v p=prefix_ -v s=_suffix 'echo {p}{}{s}'
     prefix_3_suffix
     prefix_1_suffix
     prefix_2_suffix
  2. A variable name should start with a letter and be followed by letters, digits, or underscores.
     A regular expression is used to check them: ^[a-zA-Z][A-Za-z0-9_]*$
  3. The value could also contain replacement strings.
     # {p} will be replaced with {%%:}, which computes the basename and remove all file extensions.
     $ echo a/b/c.txt.gz | rush -v 'p={%%:}' 'echo {p} {p}.csv'
     c c.csv

`, VERSION),
	Run: func(cmd *cobra.Command, args []string) {
		process.Log = log
		var err error
		config := getConfigs(cmd)

		if len(config.Infiles) == 0 {
			config.Infiles = append(config.Infiles, "-")
		}

		if config.Version {
			checkVersion()
			return
		}

		if len(config.Infiles) == 1 && isStdin(config.Infiles[0]) && !xopen.IsStdin() {
			checkError(fmt.Errorf(`STDIN not detected. type "rush -h" for help`))
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
			if runtime.GOOS == "windows" {
				command0 = `echo {}`
			} else {
				command0 = `echo "{}"`
			}
		}

		// -----------------------------------------------------------------

		// out file handler
		var outfh *os.File = nil
		var errfh *os.File = nil
		if isStdin(config.OutFile) {
			// use unbuffered file for writing, so we don't lose output on .Exit()
			outfh = os.Stdout
			errfh = os.Stderr
		} else {
			// use unbuffered file for writing, so we don't lose output on .Exit()
			outfh, err = os.Create(config.OutFile)
			checkError(err)
			defer outfh.Close()
			errfh = outfh
		}

		// TmpOutputDataBuffer = config.BufferSize
		if config.DryRun {
			config.Verbose = false
		}

		var succCmds = make(map[string]struct{})
		var bfhSuccCmds *bufio.Writer
		if config.Continue {
			var existed bool
			existed, err = exists(config.SuccCmdFile)
			checkError(err)
			var fhSuccCmds *os.File
			if existed {
				succCmds = readSuccCmds(config.SuccCmdFile)
				fhSuccCmds, err = os.OpenFile(config.SuccCmdFile, os.O_APPEND|os.O_WRONLY, 0664)
			} else {
				fhSuccCmds, err = os.Create(config.SuccCmdFile)
			}
			checkError(err)

			defer fhSuccCmds.Close()

			bfhSuccCmds = bufio.NewWriter(fhSuccCmds)
		}

		opts := &process.Options{
			DryRun:              config.DryRun,
			Jobs:                config.Jobs,
			ETA:                 config.ETA,
			KeepOrder:           config.KeepOrder,
			Retries:             config.Retries,
			RetryInterval:       time.Duration(config.RetryInterval) * time.Second,
			OutFileHandle:       outfh,
			ErrFileHandle:       errfh,
			ImmediateOutput:     config.ImmediateOutput,
			PrintRetryOutput:    config.PrintRetryOutput,
			Timeout:             time.Duration(config.Timeout) * time.Second,
			StopOnErr:           config.StopOnErr,
			NoStopExes:          config.NoStopExes,
			NoKillExes:          config.NoKillExes,
			CleanupTime:         time.Duration(config.CleanupTime) * time.Second,
			PropExitStatus:      config.PropExitStatus,
			Verbose:             config.Verbose,
			RecordSuccessfulCmd: config.Continue,
		}

		// split function for scanner
		recordDelimiter := []byte(config.RecordDelimiter)
		split := func(data []byte, atEOF bool) (advance int, token []byte, err error) {
			if atEOF && len(data) == 0 {
				return 0, nil, nil
			}
			if len(recordDelimiter) == 0 {
				return 1, data[0:1], nil
			}
			i := bytes.Index(data, recordDelimiter)
			if i >= 0 {
				return i + len(recordDelimiter), data[0:i], nil // trim config.RecordDelimiter
			}
			if atEOF {
				return len(data), data, nil
			}
			return 0, nil, nil
		}

		// ---------------------------------------------------------------

		cancel := make(chan struct{})

		donePreprocessFiles := make(chan int)

		// channel of command
		chCmdStr := make(chan string, 1)

		// run
		chOutput, chSuccessfulCmd, doneSendOutput, chExitStatus := process.Run4Output(opts, cancel, chCmdStr)

		doneCheckSuccCmd := make(chan int)
		var nSuccCmds int
		go func() {
			for c := range chSuccessfulCmd {
				nSuccCmds++
				if config.Continue {
					bfhSuccCmds.WriteString(c + endMarkOfCMD)
					bfhSuccCmds.Flush()
				}
			}
			doneCheckSuccCmd <- 1
		}()

		// generate commands

		anyCommands := false

		var inputlines []string
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
			scanner := bufio.NewScanner(infh)
			// 2147483647: max int32
			scanner.Buffer(make([]byte, 0, 16384), 2147483647)
			scanner.Split(split)

			for scanner.Scan() {
				select {
				case <-cancel:
					if config.Verbose {
						log.Warningf("cancel reading file: %s", file)
					}
				default:
				}
				inputlines = append(inputlines, scanner.Text())
			}
			checkError(errors.Wrap(scanner.Err(), "read input data"))
		}

		if opts.ETA {
			jobcount := len(inputlines) / config.NRecords
			if len(inputlines)%config.NRecords > 0 {
				jobcount++
			}
			opts.ETABar = pb.NewOptions(jobcount,
				pb.OptionSetWriter(os.Stderr),
				pb.OptionShowCount(),
				pb.OptionShowIts(),
				pb.OptionSetItsString("jobs"),
				pb.OptionSetRenderBlankState(true),
				pb.OptionSetTheme(pb.Theme{
					Saucer:        "=",
					SaucerHead:    ">",
					SaucerPadding: "_",
					BarStart:      "[",
					BarEnd:        "]",
				}))
		}

		// read data and generate command
		go func() {
			n := config.NRecords
			var id uint64 = 1

			var records []string
			records = make([]string, 0, n)
			var cmdStr string
			var runned bool
			nJobs := (len(inputlines) + n - 1) / n
			for _, record := range inputlines {
				if record == "" {
					continue
				}
				records = append(records, record)

				if len(records) == n {
					cmdStr, err = fillCommand(config, command0, Chunk{ID: id, Data: records}, nJobs-nSuccCmds)
					checkError(errors.Wrap(err, "fill command"))
					if config.Escape {
						cmdStr = stringutil.EscapeSymbols(cmdStr, config.EscapeSymbols)
					}
					if len(cmdStr) > 0 {
						if config.Continue {
							if _, runned = succCmds[cmdStr]; runned {
								log.Infof("ignore cmd: %s", cmdStr)
								if opts.ETA {
									opts.ETABar.Add(1)
									fmt.Fprintln(os.Stderr)
								}
								// bfhSuccCmds.WriteString(cmdStr + endMarkOfCMD)
								// bfhSuccCmds.Flush()
							} else {
								chCmdStr <- cmdStr
								anyCommands = true
							}
						} else {
							chCmdStr <- cmdStr
							anyCommands = true
						}

						id++
					}
					records = make([]string, 0, n)
				}
			}
			if len(records) > 0 {
				cmdStr, err = fillCommand(config, command0, Chunk{ID: id, Data: records}, nJobs-nSuccCmds)
				checkError(errors.Wrap(err, "fill command"))
				if config.Escape {
					cmdStr = stringutil.EscapeSymbols(cmdStr, config.EscapeSymbols)
				}
				if len(cmdStr) > 0 {
					if config.Continue {
						if _, runned = succCmds[cmdStr]; runned {
							log.Infof("ignore cmd: %s", cmdStr)
							// bfhSuccCmds.WriteString(cmdStr + endMarkOfCMD)
							// bfhSuccCmds.Flush()
						} else {
							chCmdStr <- cmdStr
							anyCommands = true
						}
					} else {
						chCmdStr <- cmdStr
						anyCommands = true
					}
				}
			}

			close(chCmdStr)
			// if Verbose {
			// 	log.Infof("finish reading input data")
			// }
			donePreprocessFiles <- 1
		}()

		// ---------------------------------------------------------------

		// read from chOutput and print
		doneOutput := make(chan int)
		go func() {
			last := time.Now().Add(2 * time.Second)
			for c := range chOutput {
				outfh.WriteString(c)
				if t := time.Now(); t.After(last) {
					last = t.Add(2 * time.Second)
				}
			}
			doneOutput <- 1
		}()

		var pToolExitStatus *int = nil
		var doneExitStatus chan int
		if config.PropExitStatus {
			doneExitStatus = make(chan int)
			toolExitStatus := 0
			go func() {
				for childCode := range chExitStatus {
					setPointer := false
					setCode := false
					if pToolExitStatus == nil {
						setPointer = true
						setCode = true
					} else {
						// use the code from the first error we received
						if *pToolExitStatus == 0 && childCode != 0 {
							setCode = true
						}
					}
					if setPointer {
						pToolExitStatus = &toolExitStatus
					}
					if setCode {
						*pToolExitStatus = childCode
					}
				}
				doneExitStatus <- 1
			}()
		}

		// ---------------------------------------------------------------

		chExitSignalMonitor := make(chan struct{})
		signalChan := make(chan os.Signal, 1)
		cleanupDone := make(chan int)
		signal.Notify(signalChan, os.Interrupt)
		go func() {
			select {
			case <-signalChan:
				log.Criticalf("received an interrupt, stopping unfinished commands...")
				select {
				case <-cancel: // already closed
				default:
					close(cancel)
				}
				cleanupDone <- 1
				return
			case <-chExitSignalMonitor:
				cleanupDone <- 1
				return
			}
		}()

		// the order is very important!
		<-donePreprocessFiles // finish read data and send command
		<-doneSendOutput      // finish send output
		<-doneOutput          // finish print output
		if opts.ETA {
			opts.ETABar.Finish()
			os.Stderr.WriteString("\n")
		}
		if config.PropExitStatus {
			<-doneExitStatus
		}
		if config.Continue {
			<-doneCheckSuccCmd
		}

		close(chExitSignalMonitor)
		<-cleanupDone

		if config.PropExitStatus {
			if anyCommands {
				if pToolExitStatus != nil {
					os.Exit(*pToolExitStatus)
				} else {
					checkError(fmt.Errorf(`did not get an exit status int from any child process)`))
				}
			}
		}
	},
}

// Chunk contains input data records sent to a command
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
	RootCmd.Flags().BoolP("eta", "", false, `show ETA progress bar`)

	RootCmd.Flags().IntP("jobs", "j", runtime.NumCPU(), "run n jobs in parallel (default value depends on your device)")
	RootCmd.Flags().StringP("out-file", "o", "-", `out file ("-" for stdout)`)

	RootCmd.Flags().StringSliceP("infile", "i", []string{}, "input data file, multi-values supported")

	RootCmd.Flags().StringP("record-delimiter", "D", "\n", `record delimiter (default is "\n")`)
	RootCmd.Flags().StringP("records-join-sep", "J", "\n", `record separator for joining multi-records (default is "\n")`)
	RootCmd.Flags().IntP("nrecords", "n", 1, "number of records sent to a command")
	RootCmd.Flags().StringP("field-delimiter", "d", `\s+`, "field delimiter in records, support regular expression")

	RootCmd.Flags().IntP("retries", "r", 0, "maximum retries (default 0)")
	RootCmd.Flags().IntP("retry-interval", "", 0, "retry interval (unit: second) (default 0)")
	RootCmd.Flags().BoolP("immediate-output", "I", false, "print output immediately and interleaved, to aid debugging")
	RootCmd.Flags().BoolP("print-retry-output", "", true, "print output from retry commands")
	RootCmd.Flags().IntP("timeout", "t", 0, "timeout of a command (unit: seconds, 0 for no timeout) (default 0)")

	RootCmd.Flags().BoolP("keep-order", "k", false, "keep output in order of input")
	RootCmd.Flags().BoolP("stop-on-error", "e", false, "stop child processes on first error (not perfect, you may stop it by typing ctrl-c or closing terminal)")
	RootCmd.Flags().BoolP("propagate-exit-status", "", true, "propagate child exit status up to the exit status of rush")
	RootCmd.Flags().StringSliceP("no-stop-exes", "", []string{}, "exe names to exclude from stop signal, example: mspdbsrv.exe; or use all for all exes (default none)")
	RootCmd.Flags().StringSliceP("no-kill-exes", "", []string{}, "exe names to exclude from kill signal, example: mspdbsrv.exe; or use all for all exes (default none)")
	RootCmd.Flags().IntP("cleanup-time", "", 3, "time to allow child processes to clean up between stop / kill signals (unit: seconds, 0 for no time) (default 3)")
	RootCmd.Flags().BoolP("dry-run", "", false, "print command but not run")

	RootCmd.Flags().BoolP("continue", "c", false, `continue jobs.`+
		` NOTES: 1) successful commands are saved in file (given by flag -C/--succ-cmd-file);`+
		` 2) if the file does not exist, rush saves data so we can continue jobs next time;`+
		` 3) if the file exists, rush ignores jobs in it and update the file`)
	RootCmd.Flags().StringP("succ-cmd-file", "C", "successful_cmds.rush", `file for saving successful commands`)

	// RootCmd.Flags().IntP("buffer-size", "", 1, "buffer size for output of a command before saving to tmpfile (unit: Mb)")

	RootCmd.Flags().StringSliceP("assign", "v", []string{}, "assign the value val to the variable var (format: var=val, val also supports replacement strings)")
	RootCmd.Flags().StringP("trim", "T", "", `trim white space (" \t\r\n") in input (available values: "l" for left, "r" for right, "lr", "rl", "b" for both side)`)
	// RootCmd.Flags().BoolP("greedy", "g", false, `greedy replacement strings (replace again), useful for preset variable, e.g., rush -v p={:^_1} 'echo {p}'`)

	RootCmd.Flags().BoolP("escape", "q", false, `escape special symbols like $ which you can customize by flag -Q/--escape-symbols`)
	RootCmd.Flags().StringP("escape-symbols", "Q", "$#&`", "symbols to escape")

	RootCmd.Example = `  1. simple run, quoting is not necessary
      $ seq 1 10 | rush echo {}
  2. keep order
      $ seq 1 10 | rush 'echo {}' -k
  3. timeout
      $ seq 1 | rush 'sleep 2; echo {}' -t 1
  4. retry
      $ seq 1 | rush 'python script.py' -r 3
  5. dirname & basename & remove suffix
      $ echo dir/file_1.txt.gz | rush 'echo {/} {%} {^_1.txt.gz}'
      dir file.txt.gz dir/file
  6. basename without the last or any extension
      $ echo dir.d/file.txt.gz | rush 'echo {.} {:} {%.} {%:}'
      dir.d/file.txt dir.d/file file.txt file
  7. job ID, combine fields and other replacement strings
      $ echo 12 file.txt dir/s_1.fq.gz | rush 'echo job {#}: {2} {2.} {3%:^_1}'
      job 1: file.txt file s
  8. capture submatch using regular expression
      $ echo read_1.fq.gz | rush 'echo {@(.+)_\d}'
      read
  9. custom field delimiter
      $ echo a=b=c | rush 'echo {1} {2} {3}' -d =
      a b c
  10. custom record delimiter
      $ echo a=b=c | rush -D "=" -k 'echo {}'
      a
      b
      c
      $ echo abc | rush -D "" -k 'echo {}'
      a
      b
      c
  11. assign value to variable, like "awk -v"
      # seq 1 | rush 'echo Hello, {fname} {lname}!' -v fname=Wei,lname=Shen
      $ seq 1 | rush 'echo Hello, {fname} {lname}!' -v fname=Wei -v lname=Shen
      Hello, Wei Shen!

      # preset variables support extra operations as well.
      echo read_1.fq.gz | ./rush -v 'p={:^_1}' -v 'f=a.s-10.txt' 'echo {} {p} {f:} {f@s\-(\d+)}'
      read_1.fq.gz read a 10
  12. preset variable (Macro)
      # equal to: echo sample_1.fq.gz | rush 'echo {:^_1} {} {:^_1}_2.fq.gz'
      $ echo sample_1.fq.gz | rush -v p={:^_1} 'echo {p} {} {p}_2.fq.gz'
      sample sample_1.fq.gz sample_2.fq.gz
  13. save successful commands to continue in NEXT run
      $ seq 1 3 | rush 'sleep {}; echo {}' -c -t 2
      [INFO] ignore cmd #1: sleep 1; echo 1
      [ERRO] run cmd #1: sleep 2; echo 2: time out
      [ERRO] run cmd #2: sleep 3; echo 3: time out
  14. escape special symbols
      $ seq 1 | rush 'echo -e "a\tb" | awk "{print $1}"' -q
      a
  15. escape curly brackets "{}"
      $ echo aaa bbb ccc | sed -E "s/(\S){3,}/\1/g"
      a b c
      $ echo 1 | rush 'echo aaa bbb ccc | sed -E "s/(\S){{3,}}/\1/g"' --dry-run
      echo aaa bbb ccc | sed -E "s/(\S){3,}/\1/g"
  16. run a command with relative paths in Windows, please use backslash as the separator.
      # "brename -l -R" is used to search paths recursively
      $ brename -l -q -R -i -p "\.go$" | rush "bin\app.exe {}"

  More examples: https://github.com/shenwei356/rush`

	RootCmd.SetUsageTemplate(`Usage:{{if .Runnable}}
  {{if .HasAvailableFlags}}{{appendIfNotPresent .UseLine "[flags]"}}{{else}}{{.UseLine}}{{end}}{{end}}{{if .HasAvailableSubCommands}}
  {{ .CommandPath}} [command]{{end}} [command] {{if gt .Aliases 0}}

Aliases:
  {{.NameAndAliases}}
{{end}}{{if .HasExample}}

Examples:
{{ .Example }}{{end}}{{ if .HasAvailableSubCommands}}

Available Commands:{{range .Commands}}{{if .IsAvailableCommand}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}{{end}}{{ if .HasAvailableLocalFlags}}

Flags:
{{.LocalFlags.FlagUsagesWrapped 110 | trimTrailingWhitespaces}}{{end}}{{if .HasAvailableInheritedFlags}}

Global Flags:
{{.InheritedFlags.FlagUsagesWrapped 110 | trimTrailingWhitespaces}}{{end}}{{if .HasHelpSubCommands}}

Additional help topics:{{range .Commands}}{{if .IsHelpCommand}}
  {{rpad .CommandPath .CommandPathPadding}} {{.Short}}{{end}}{{end}}{{end}}{{ if .HasAvailableSubCommands }}

Use "{{.CommandPath}} [command] --help" for more information about a command.{{end}}
`)
}

// Config is the struct containing all global flags
type Config struct {
	Verbose bool
	Version bool
	ETA     bool

	Jobs    int
	OutFile string

	Infiles []string

	RecordDelimiter      string
	RecordsJoinSeparator string
	NRecords             int
	FieldDelimiter       string
	reFieldDelimiter     *regexp.Regexp

	Retries          int
	RetryInterval    int
	ImmediateOutput  bool
	PrintRetryOutput bool
	Timeout          int

	KeepOrder      bool
	StopOnErr      bool
	NoStopExes     []string
	NoKillExes     []string
	CleanupTime    int
	PropExitStatus bool
	DryRun         bool

	Continue    bool
	SuccCmdFile string

	AssignMap   map[string]string
	Trim        string
	Greedy      bool
	GreedyCount int

	Escape        bool
	EscapeSymbols string
}

// var=value
var reAssign = regexp.MustCompile(`\s*([a-zA-Z][A-Za-z0-9_]*)\s*=(.+)`)

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
		if reAssign.MatchString(s) {
			found := reAssign.FindStringSubmatch(s)
			assignMap[found[1]] = found[2]
		} else {
			checkError(fmt.Errorf(`illegal value for flag -v/--assign (format: "var=value", type "rush -h" for more details): %s`, s))
		}
	}

	return Config{
		Verbose: getFlagBool(cmd, "verbose"),
		Version: getFlagBool(cmd, "version"),
		ETA:     getFlagBool(cmd, "eta"),

		Jobs:    getFlagPositiveInt(cmd, "jobs"),
		OutFile: getFlagString(cmd, "out-file"),

		Infiles: getFlagStringSlice(cmd, "infile"),

		RecordDelimiter:      getFlagString(cmd, "record-delimiter"),
		RecordsJoinSeparator: getFlagString(cmd, "records-join-sep"),
		NRecords:             getFlagPositiveInt(cmd, "nrecords"),
		FieldDelimiter:       getFlagString(cmd, "field-delimiter"),

		Retries:          getFlagNonNegativeInt(cmd, "retries"),
		RetryInterval:    getFlagNonNegativeInt(cmd, "retry-interval"),
		ImmediateOutput:  getFlagBool(cmd, "immediate-output"),
		PrintRetryOutput: getFlagBool(cmd, "print-retry-output"),
		Timeout:          getFlagNonNegativeInt(cmd, "timeout"),

		KeepOrder:      getFlagBool(cmd, "keep-order"),
		StopOnErr:      getFlagBool(cmd, "stop-on-error"),
		NoStopExes:     getFlagStringSlice(cmd, "no-stop-exes"),
		NoKillExes:     getFlagStringSlice(cmd, "no-kill-exes"),
		CleanupTime:    getFlagNonNegativeInt(cmd, "cleanup-time"),
		PropExitStatus: getFlagBool(cmd, "propagate-exit-status"),
		DryRun:         getFlagBool(cmd, "dry-run"),

		Continue:    getFlagBool(cmd, "continue"),
		SuccCmdFile: getFlagString(cmd, "succ-cmd-file"),

		Trim:        trim,
		AssignMap:   assignMap,
		Greedy:      true, // getFlagBool(cmd, "greedy"),
		GreedyCount: 1,

		Escape:        getFlagBool(cmd, "escape"),
		EscapeSymbols: getFlagString(cmd, "escape-symbols"),
	}
}
