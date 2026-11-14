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
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/signal"
	"regexp"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	"github.com/pkg/errors"
	pb "github.com/schollz/progressbar/v3"
	"github.com/shenwei356/rush/internal/runstate"
	"github.com/shenwei356/rush/process"
	"github.com/shenwei356/xopen"
	"github.com/spf13/cobra"
)

var writeSuccessfulCommandFile = func(w *bufio.Writer, value string) (int, error) { return w.WriteString(value) }
var flushSuccessfulCommandFile = func(w *bufio.Writer) error { return w.Flush() }
var closeRunFile = func(f *os.File) error { return f.Close() }
var truncateSuccessfulCommandFile = func(f *os.File, size int64) error { return f.Truncate(size) }
var seekSuccessfulCommandFile = func(f *os.File, offset int64, whence int) (int64, error) {
	return f.Seek(offset, whence)
}

func appendSuccessfulCommand(w *bufio.Writer, command string) error {
	if _, err := writeSuccessfulCommandFile(w, encodeSuccessfulCommand(command)+endMarkOfCMD); err != nil {
		return err
	}
	return flushSuccessfulCommandFile(w)
}

func rollbackSuccessfulCommands(w *bufio.Writer, f *os.File, size int64) error {
	w.Reset(f)
	if err := truncateSuccessfulCommandFile(f, size); err != nil {
		return err
	}
	_, err := seekSuccessfulCommandFile(f, 0, os.SEEK_END)
	return err
}

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
        --pipe              send each group of records to the command's standard input
    -T, --trim              trim white space (" \t\r\n") in input

Output:
  - Outputs of all commands are written to STDOUT by default,
    you can also use -o/--out-file to specify an output file.
    Output and successful-command files must differ from each other and from inputs.
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

  {#} and {?} must stand alone inside their braces. Missing fields, undefined
  variables, and unsupported expressions stay unchanged.

  With --continue, {#} and {?} are kept stable in the successful-command file,
  so changing input order or job count does not rerun otherwise unchanged jobs.
  The record file must be regular; concurrent writers and ambiguous or damaged
  records are rejected. Review completed jobs before rebuilding rejected records.
  --dry-run never creates or updates the record file.

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

  For multi-value flags (-i, -v, --no-stop-exes, --no-kill-exes), repeat the flag
  or use CSV quoting, e.g., -v '"p=a,b"' for a value containing a comma.

Waiting before starting jobs:
  - -W/--wait-for-process NAME waits for a process name to appear;
    -w/--wait-for-process-exit NAME waits until all matching processes exit.
  - -U/--wait-for-process-duration sets how many seconds the condition must remain
    satisfied (default 0). A contrary observation restarts the timer.
  - -u/--wait-for-process-interval sets the seconds between checks (default 1;
    supports fractions). Processes appearing and exiting between checks can be
    missed. Shorter intervals reduce this risk but increase scanning overhead.
  - Names match exactly by default. -R/--wait-for-process-regexp enables regular
    expressions (partial matching; use ^ and $ for a whole name). Matching ignores
    case by default on Windows. Use the process name, not a command line or path.
    Checks exclude rush itself.
  - The condition is checked only before the run. Jobs then follow -j and other
    start limits. --dry-run skips waiting; Ctrl+C cancels it.

Resource limits:
  - -j/--jobs limits concurrent jobs. --delay 0.5 leaves at least 0.5 seconds
    between job starts.
  - --load 4 starts jobs only while the one-minute system load is below 4.
    --load 100%% uses the CPU count as the threshold.
  - --memfree 1G waits for at least 1 GiB of available physical memory;
    0 disables this limit. Uppercase size suffixes use powers of 1024,
    and lowercase suffixes use powers of 1000.
  - Below half the --memfree threshold, rush stops the youngest running job
    and requeues it without consuming a retry attempt. Its buffered stdout
    is discarded; stderr already written remains visible.
  - These limits apply to starts and retries. --dry-run validates the options
    without waiting for resources.
  - Concurrent jobs (Unix): rush reduces concurrency to fit its open-file limit
    and reports a warning. If one job cannot fit, raise ulimit -n or reduce --retries.
  - Commands opening many files or using many threads: child programs' own
    file and thread limits still apply.
  - Background work and retries: failed commands' children are stopped before
    retrying. Shell commands that launch background work should wait for it.

`, VERSION),
	Run: func(cmd *cobra.Command, args []string) {
		process.Log = log
		var err error
		config := getConfigs(cmd)

		if len(config.Infiles) == 0 {
			config.Infiles = append(config.Infiles, "-")
		}

		if config.Version {
			checkError(protectRunStream(config, os.Stdout))
			checkVersion()
			return
		}

		if len(config.Infiles) == 1 && isStdin(config.Infiles[0]) && !xopen.IsStdin() {
			checkError(fmt.Errorf(`STDIN not detected. type "rush -h" for help`))
		}

		j := runtime.NumCPU()
		if config.Jobs < j {
			j = config.Jobs + 1
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

		checkError(validateRunFiles(config))
		if config.ProcessWait.name != "" && !config.DryRun {
			status, waitErr := waitBeforeRun(config.ProcessWait, config.Verbose)
			checkError(waitErr)
			if status != 0 {
				os.Exit(status)
			}
		}

		// TmpOutputDataBuffer = config.BufferSize
		if config.DryRun {
			config.Verbose = false
		}

		var succCmds = make(map[string]struct{})
		var hasOlderRecords bool
		var bfhSuccCmds *bufio.Writer
		var fhSuccCmds *os.File
		var succCmdOriginalSize int64
		if config.Continue {
			// Lock before reading records or opening output: rollback must not
			// erase another run's successes, and previews must not read torn writes.
			recordFile, openErr := openContinueFile(config.SuccCmdFile, config.DryRun)
			checkError(openErr)
			if recordFile != nil {
				defer recordFile.Close()
				succCmds, hasOlderRecords = readSuccCmdsFrom(recordFile)
				if config.DryRun {
					checkError(recordFile.Close())
				} else {
					fhSuccCmds = recordFile
					_, seekErr := fhSuccCmds.Seek(0, os.SEEK_END)
					checkError(seekErr)
					info, statErr := fhSuccCmds.Stat()
					checkError(statErr)
					succCmdOriginalSize = info.Size()
					bfhSuccCmds = bufio.NewWriter(fhSuccCmds)
				}
			}
		}
		// out file handler
		var outfh *os.File = nil
		var errfh *os.File = nil
		if isStdin(config.OutFile) {
			// use unbuffered file for writing, so we don't lose output on .Exit()
			outfh = os.Stdout
			errfh = os.Stderr
		} else {
			// use unbuffered file for writing, so we don't lose output on .Exit()
			// Open without truncation until paths, setup, and input reading succeed.
			outfh, err = os.OpenFile(config.OutFile, os.O_WRONLY|os.O_CREATE, 0666)
			checkError(err)
			defer outfh.Close()
			errfh = outfh
		}

		// New files may expose aliases through symlinked paths. Check again
		// before truncating the output or appending any successful commands.
		checkError(validateRunFiles(config))
		// Queue interrupts before changing inherited output flags. The monitor
		// below handles them once the run state and process options are ready.
		signalChan := make(chan os.Signal, 2)
		signal.Notify(signalChan, terminationSignals()...)
		defer signal.Stop(signalChan)
		originalOutput := outfh
		outfh, restoreOutput, outputSetupErr := newInterruptibleOutput(outfh)
		checkError(errors.Wrap(outputSetupErr, "prepare interruptible output"))
		defer restoreOutput()
		diagnostics, restoreDiagnostics, diagnosticsErr := prepareDiagnostics()
		if diagnosticsErr != nil {
			_ = restoreOutput()
			checkError(errors.Wrap(diagnosticsErr, "prepare interruptible stderr"))
		}
		defer restoreDiagnostics()
		if errfh == os.Stderr {
			errfh = diagnostics
		}
		if errfh == originalOutput {
			errfh = outfh
		}
		// Input failures exit before normal shutdown; restore inherited output
		// flags here too, while no command has started using the output yet.
		checkInputError := func(err error) {
			if err != nil {
				// Log while deadlines still work: reporting a read failure can
				// itself block on stderr and require a second interrupt.
				log.Error(err)
				_ = restoreDiagnostics()
				_ = restoreOutput()
				os.Exit(1)
			}
		}

		opts := &process.Options{
			DryRun:           config.DryRun,
			Jobs:             config.Jobs,
			ETA:              config.ETA,
			KeepOrder:        config.KeepOrder,
			Retries:          config.Retries,
			RetryInterval:    config.RetryInterval,
			OutFileHandle:    outfh,
			ErrFileHandle:    errfh,
			ImmediateOutput:  config.ImmediateOutput,
			PrintRetryOutput: config.PrintRetryOutput,
			Timeout:          config.Timeout,
			StopOnErr:        config.StopOnErr,
			NoStopExes:       config.NoStopExes,
			NoKillExes:       config.NoKillExes,
			CleanupTime:      config.CleanupTime,
			StartDelay:       config.StartDelay,
			MaxLoad:          config.MaxLoad,
			MinFreeMemory:    config.MinFreeMemory,
			// Always collect statuses here so timeout retains its documented 124
			// even when ordinary child-status propagation is disabled.
			PropExitStatus:      true,
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
				return i + len(recordDelimiter), data[0 : i+len(recordDelimiter)], nil
			}
			if atEOF {
				return len(data), data, nil
			}
			return 0, nil, nil
		}

		// ---------------------------------------------------------------

		state, runCtx := runstate.New(context.TODO())
		cancelRun := state.Cancel
		defer cancelRun()

		chExitSignalMonitor := make(chan struct{})
		cleanupDone := make(chan int)
		var forcedOutputStop atomic.Bool // Also guards final progress output.
		// A full stderr pipe must not prevent the monitor receiving another
		// interrupt. Drain notices separately while keeping escalation active.
		signalMessages := make(chan string, 2)
		signalLogsDone := make(chan struct{})
		go func() {
			defer close(signalLogsDone)
			for message := range signalMessages {
				log.Critical(message)
			}
		}()
		go func() {
			interrupted := false
			stopMonitor := chExitSignalMonitor
			var logsDone <-chan struct{}
			notify := func(message string) {
				if stopMonitor != nil {
					select {
					case signalMessages <- message:
					default: // Repeated interrupts need no unbounded log queue.
					}
				}
			}
			for {
				select {
				case sig := <-signalChan:
					if interrupted {
						state.Force()
						opts.ForceStop()
						forcedOutputStop.Store(true)
						// Release command output, diagnostics, and any logger
						// holding the logging mutex while its pipe is full.
						_ = outfh.SetWriteDeadline(time.Now())
						_ = diagnostics.SetWriteDeadline(time.Now())
						notify("received another interrupt, killing unfinished commands...")
						continue
					}
					interrupted = true
					state.Stop(runstate.Cause{Kind: runstate.Signal, Status: terminationStatus(sig)})
					notify("received an interrupt, stopping unfinished commands...")
				case <-stopMonitor:
					close(signalMessages)
					stopMonitor = nil
					logsDone = signalLogsDone
				case <-logsDone:
					cleanupDone <- 1
					return
				}
			}
		}()

		donePreprocessFiles := make(chan int)

		// channel of command
		chCmdStr := make(chan process.Job, 1)

		// run
		chOutput, chSuccessfulCmd, doneSendOutput, chExitStatus := process.Run4OutputContextJobs(opts, runCtx, cancelRun, chCmdStr)

		doneCheckSuccCmd := make(chan int)
		var nSuccCmds atomic.Int64
		var succCmdWriteErr error
		go func() {
			for c := range chSuccessfulCmd {
				nSuccCmds.Add(1)
				if bfhSuccCmds != nil {
					if succCmdWriteErr != nil {
						continue
					}
					succCmdWriteErr = appendSuccessfulCommand(bfhSuccCmds, c)
					if succCmdWriteErr != nil {
						state.Stop(runstate.Cause{Kind: runstate.Internal, Status: 1})
						log.Error(errors.Wrap(succCmdWriteErr, "write successful-command file"))
					}
				}
			}
			close(doneCheckSuccCmd)
		}()

		// generate commands

		anyCommands := false

		var inputlines []inputRecord
		for _, file := range config.Infiles {
			if runCtx.Err() != nil {
				break
			}
			// input file handler
			var infh *os.File
			if isStdin(file) {
				infh = os.Stdin
			} else {
				infh, err = openInputFile(file)
				checkInputError(err)
			}
			inputReader, restoreInput, readerErr := newInputReader(infh, runCtx)
			if readerErr != nil && !isStdin(file) {
				_ = infh.Close()
			}
			checkInputError(errors.Wrap(readerErr, "prepare input reader"))
			scanner := bufio.NewScanner(inputReader)
			inputCloserDone := make(chan struct{})
			inputCloserExited := make(chan struct{})
			go func(f *os.File) {
				defer close(inputCloserExited)
				select {
				case <-runCtx.Done():
					_ = f.Close()
				case <-inputCloserDone:
				}
			}(infh)
			// 2147483647: max int32
			scanner.Buffer(make([]byte, 0, 16384), 2147483647)
			scanner.Split(split)

			for scanner.Scan() {
				if runCtx.Err() != nil {
					if config.Verbose {
						log.Warningf("cancel reading file: %s", file)
					}
					break
				}
				record := scanner.Text()
				terminated := false
				if len(recordDelimiter) > 0 && strings.HasSuffix(record, config.RecordDelimiter) {
					record = record[:len(record)-len(config.RecordDelimiter)]
					terminated = true
				}
				// Empty records are ignored by batching, so exclude them from
				// both stored input and the initial progress total.
				if record == "" {
					continue
				}
				inputlines = append(inputlines, inputRecord{data: record, terminated: terminated})
			}
			close(inputCloserDone)
			<-inputCloserExited
			if restoreErr := restoreInput(); restoreErr != nil {
				state.Stop(runstate.Cause{Kind: runstate.Internal, Status: 1})
				log.Error(errors.Wrap(restoreErr, "restore input flags"))
			}
			// All input is read before scheduling. Release each file now rather
			// than retaining one descriptor per -i argument until rush exits.
			if !isStdin(file) {
				closeErr := infh.Close()
				if closeErr != nil && runCtx.Err() == nil {
					checkInputError(errors.Wrap(closeErr, "close input file"))
				}
			}
			if scanErr := scanner.Err(); scanErr != nil && runCtx.Err() == nil {
				state.Stop(runstate.Cause{Kind: runstate.Internal, Status: 1})
				checkInputError(errors.Wrap(scanErr, "read input data"))
			}
		}

		// Initialize output only after the first runnable command is expanded.
		// The producer owns this state until it finishes; an empty successful
		// run initializes output below after all goroutines have been joined.
		outputReady := false
		prepareOutput := func() error {
			if outputReady {
				return nil
			}
			if !isStdin(config.OutFile) {
				info, err := outfh.Stat()
				if err != nil {
					return err
				}
				if info.Mode().IsRegular() {
					if err := outfh.Truncate(0); err != nil {
						return err
					}
				}
			}
			outputReady = true
			return nil
		}

		if opts.ETA {
			jobcount := len(inputlines) / config.NRecords
			if len(inputlines)%config.NRecords > 0 {
				jobcount++
			}
			opts.ETABar = pb.NewOptions(jobcount,
				pb.OptionSetWriter(diagnostics),
				pb.OptionSetDescription("processed jobs: "),
				pb.OptionShowCount(),
				pb.OptionShowIts(),
				pb.OptionShowElapsedTimeOnFinish(),
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
			defer close(chCmdStr)
			defer close(donePreprocessFiles)

			// Exiting from this producer would abandon active children. Cancel
			// through the shared state so cleanup and output draining can finish.
			stopForExpansionError := func(err error, operation string) {
				state.Stop(runstate.Cause{Kind: runstate.InputError, Status: 1})
				log.Error(errors.Wrap(err, operation))
			}

			sendCommand := func(cmdStr, recordCmd, stdin string) bool {
				if runCtx.Err() != nil {
					return false
				}
				if err := prepareOutput(); err != nil {
					state.Stop(runstate.Cause{Kind: runstate.Internal, Status: 1})
					log.Error(errors.Wrap(err, "prepare command output"))
					return false
				}
				select {
				case chCmdStr <- process.Job{Cmd: cmdStr, RecordCmd: recordCmd, Stdin: stdin}:
					anyCommands = true
					return true
				case <-runCtx.Done():
					return false
				}
			}
			// Older formats can lose input or job-value dependencies when macros
			// construct another placeholder. Do not guess which jobs they identify.
			if hasOlderRecords {
				if err := validateLegacyContinueExpansion(runCtx, config, command0, inputlines); err != nil {
					if runCtx.Err() == nil {
						stopForExpansionError(err, "match successful commands")
					}
					return
				}
			}
			wasSuccessful := func(recordCmd, cmdStr string) (bool, error) {
				composed := isComposedContinueRecord(recordCmd)
				if composed && hasOlderRecords {
					return false, fmt.Errorf("ambiguous older successful-command records for composed job placeholders: review completed jobs and use a new record file")
				}
				if _, ok := succCmds[recordCmd]; ok {
					return true, nil
				}
				if config.Pipe || composed {
					return false, nil
				}
				// Older successful-command files contain the expanded {#} value.
				_, ok := succCmds[cmdStr]
				return ok, nil
			}

			n := config.NRecords
			var id uint64 = 1
			jobOccurrences := make(continueJobOccurrences)
			nJobs := len(inputlines) / n
			if len(inputlines)%n != 0 {
				nJobs++
			}
			// Input reading already excludes empty records. Use the same path
			// for full batches and the final partial batch.
			for start := 0; start < len(inputlines); {
				if runCtx.Err() != nil {
					return
				}
				end := start + min(n, len(inputlines)-start)
				batch := inputlines[start:end]
				start = end
				records := make([]string, len(batch))
				for i := range batch {
					records[i] = batch[i].data
				}
				chunk := Chunk{ID: id, Data: records}
				cmdStr, err := fillCommand(config, command0, chunk, nJobs-int(nSuccCmds.Load()))
				if err != nil {
					stopForExpansionError(err, "fill command")
					return
				}
				if cmdStr == "" {
					if opts.ETA {
						opts.ETABar.AddMax(-1)
					}
					continue
				}
				id++
				recordCmd := cmdStr
				if config.Continue {
					recordCmd, err = fillCommandForContinue(config, command0, chunk, nJobs-int(nSuccCmds.Load()))
					if err != nil {
						stopForExpansionError(err, "fill command for continue")
						return
					}
				}
				if config.Escape {
					cmdStr = escapeSymbols(cmdStr, config.EscapeSymbols)
					recordCmd = escapeContinueCommand(recordCmd, config.EscapeSymbols)
				}
				stdin := ""
				if config.Pipe {
					stdin = buildPipeInput(batch, config.RecordDelimiter, config.Trim)
					if config.Continue {
						recordCmd = pipeContinueKey(recordCmd, stdin)
					}
				}
				if config.Continue {
					recordCmd = jobContinueKey(recordCmd, records, config.Trim, jobOccurrences)
					runned, err := wasSuccessful(recordCmd, cmdStr)
					if err != nil {
						stopForExpansionError(err, "match successful command")
						return
					}
					if runned {
						if config.Verbose {
							log.Infof("ignore cmd: %s", cmdStr)
						}
						if opts.ETA {
							opts.ETABar.Add(1)
							fmt.Fprintln(diagnostics)
						}
						continue
					}
				}
				if !sendCommand(cmdStr, recordCmd, stdin) {
					return
				}
			}
		}()

		// ---------------------------------------------------------------

		// read from chOutput and print
		doneOutput := make(chan int)
		var outputWriteErr error
		go func() {
			last := time.Now().Add(2 * time.Second)
			for c := range chOutput {
				if outputWriteErr == nil {
					_, outputWriteErr = outfh.WriteString(c)
					if outputWriteErr != nil {
						state.Stop(runstate.Cause{Kind: runstate.Internal, Status: 1})
						log.Error(errors.Wrap(outputWriteErr, "write command output"))
					}
				}
				if t := time.Now(); t.After(last) {
					last = t.Add(2 * time.Second)
				}
			}
			doneOutput <- 1
		}()

		var pToolExitStatus *int = nil
		var doneExitStatus chan int
		doneExitStatus = make(chan int)
		toolExitStatus := 0
		go func() {
			for childCode := range chExitStatus {
				// Ignore disabled statuses before choosing the first failure so
				// an earlier ordinary error cannot hide a later timeout.
				if !config.PropExitStatus && childCode != 124 {
					childCode = 0
				}
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

		// ---------------------------------------------------------------

		// the order is very important!
		<-donePreprocessFiles // finish read data and send command
		<-doneSendOutput      // finish send output
		<-doneOutput          // finish print output
		<-doneExitStatus
		if config.Continue {
			<-doneCheckSuccCmd
		}
		if state.Cause().Status == 0 && !outputReady {
			// Successful empty input or an entirely skipped run still produces
			// an empty output file, matching an ordinary successful invocation.
			if err := prepareOutput(); err != nil {
				log.Error(errors.Wrap(err, "prepare command output"))
				state.Record(runstate.Cause{Kind: runstate.Internal, Status: 1})
			}
		}

		// All writers have finished. Check Close before os.Exit bypasses
		// deferred cleanup, and before deciding whether to retain success records.
		if !isStdin(config.OutFile) {
			if closeErr := closeRunFile(originalOutput); closeErr != nil {
				log.Error(errors.Wrap(closeErr, "close command output"))
				if outputWriteErr == nil {
					outputWriteErr = closeErr
				}
				state.Record(runstate.Cause{Kind: runstate.Internal, Status: 1})
			}
		}
		cause := state.Cause()
		if fhSuccCmds != nil && (cause.Kind == runstate.Internal || outputWriteErr != nil || succCmdWriteErr != nil) {
			// Discard buffered bytes before restoring the exact pre-run length.
			if rollbackErr := rollbackSuccessfulCommands(bfhSuccCmds, fhSuccCmds, succCmdOriginalSize); rollbackErr != nil {
				log.Error(errors.Wrap(rollbackErr, "rollback successful-command file"))
				cause = runstate.Cause{Kind: runstate.Internal, Status: 1}
			}
		}
		if fhSuccCmds != nil {
			if closeErr := closeRunFile(fhSuccCmds); closeErr != nil {
				log.Error(errors.Wrap(closeErr, "close successful-command file"))
				if cause.Status == 0 {
					cause = runstate.Cause{Kind: runstate.Internal, Status: 1}
				}
			}
		}
		if opts.ETA && !forcedOutputStop.Load() {
			if opts.ETABar.GetMax() == 0 {
				// The progress library cannot render a zero maximum after it
				// changes, so replace any obsolete nonzero total explicitly.
				opts.ETABar.Clear()
				fmt.Fprintln(diagnostics, "processed jobs: (0/0)")
			} else if cause.Status == 0 {
				// Only fill the bar after a normal, uninterrupted completion.
				opts.ETABar.Finish()
			}
			diagnostics.WriteString("\n")
		}
		// Keep escalation available through final diagnostics and progress
		// output. The monitor also drains its notice writer before exiting.
		close(chExitSignalMonitor)
		<-cleanupDone
		// Restore in reverse setup order: stdout and stderr can share one
		// open-file description, even when they have different descriptor IDs.
		diagnosticRestoreErr := restoreDiagnostics()
		outputRestoreErr := restoreOutput()
		signal.Stop(signalChan)
		for len(signalChan) > 0 {
			sig := <-signalChan
			state.Record(runstate.Cause{Kind: runstate.Signal, Status: terminationStatus(sig)})
		}
		for _, restoreErr := range []error{diagnosticRestoreErr, outputRestoreErr} {
			if restoreErr != nil {
				log.Error(errors.Wrap(restoreErr, "restore output flags"))
				state.Record(runstate.Cause{Kind: runstate.Internal, Status: 1})
			}
		}
		if cause.Status == 0 {
			cause = state.Cause()
		}

		if cause.Status != 0 {
			os.Exit(cause.Status)
		}

		if anyCommands {
			if pToolExitStatus != nil {
				if *pToolExitStatus == 124 || config.PropExitStatus {
					os.Exit(*pToolExitStatus)
				}
			} else {
				checkError(fmt.Errorf(`did not get an exit status int from any child process)`))
			}
		}
	},
}

type inputRecord struct {
	data       string
	terminated bool
}

// trimInputRecord applies the same whitespace rules to replacements and stdin.
func trimInputRecord(record, mode string) string {
	switch mode {
	case "l":
		return strings.TrimLeft(record, " \t\r\n")
	case "r":
		return strings.TrimRight(record, " \t\r\n")
	case "lr", "rl", "b":
		return strings.Trim(record, " \t\r\n")
	default:
		return record
	}
}

func buildPipeInput(records []inputRecord, delimiter, trim string) string {
	var buf strings.Builder
	for i, record := range records {
		// Trim contents without changing grouping or original terminators.
		buf.WriteString(trimInputRecord(record.data, trim))
		if delimiter != "" && (record.terminated || i+1 < len(records)) {
			buf.WriteString(delimiter)
		}
	}
	return buf.String()
}

func pipeContinueKey(command, stdin string) string {
	digest := sha256.Sum256([]byte(stdin))
	// The prefix keeps stdin metadata distinct from an ordinary shell comment.
	if strings.HasPrefix(command, continueComposedCommandPrefix) {
		return fmt.Sprintf("%s%s\n# rush --pipe stdin sha256: %x", continueComposedPipePrefix, strings.TrimPrefix(command, continueComposedCommandPrefix), digest)
	}
	command = strings.TrimPrefix(command, continueCommandPrefix)
	return fmt.Sprintf("%s%s\n# rush --pipe stdin sha256: %x", continuePipePrefix, command, digest)
}

// Chunk contains input data records sent to a command
type Chunk struct {
	ID   uint64
	Data []string
}

// Execute adds all child commands to the root command sets flags appropriately.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute() {
	executeArgs(os.Args[1:])
}

// executeArgs protects diagnostic destinations before Cobra can report errors.
func executeArgs(args []string) {
	config := diagnosticConfig(RootCmd.Flags(), args)
	checkError(protectRunStream(config, os.Stderr))
	// Help bypasses Run, so protect its output without validating run options.
	help := RootCmd.HelpFunc()
	RootCmd.SetHelpFunc(func(cmd *cobra.Command, args []string) {
		checkError(protectRunStream(config, os.Stdout))
		help(cmd, args)
	})
	defer RootCmd.SetHelpFunc(help)
	RootCmd.SetArgs(args)
	if err := RootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(-1)
	}
}

func init() {
	RootCmd.Flags().BoolP("verbose", "", false, "print verbose information")
	RootCmd.Flags().BoolP("version", "V", false, `print version information and check for update`)
	RootCmd.Flags().BoolP("eta", "", false, `show ETA, processed job counts, and elapsed time`)

	RootCmd.Flags().IntP("jobs", "j", runtime.NumCPU(), "run n jobs in parallel (default value depends on your device)")
	RootCmd.Flags().Float64("delay", 0, "minimum seconds between starting jobs (supports fractions)")
	RootCmd.Flags().StringP("wait-for-process", "W", "", "wait for a process name to appear before starting any jobs")
	RootCmd.Flags().StringP("wait-for-process-exit", "w", "", "wait until no process has this name before starting any jobs")
	RootCmd.Flags().BoolP("wait-for-process-regexp", "R", false, "interpret the waiting process name as a regular expression")
	RootCmd.Flags().Float64P("wait-for-process-duration", "U", 0, "seconds the process condition must stay satisfied (supports fractions)")
	RootCmd.Flags().Float64P("wait-for-process-interval", "u", 1, "seconds between process checks (positive, supports fractions)")
	RootCmd.Flags().String("load", "", "start jobs only while system load is below this value (number or percent of CPUs)")
	RootCmd.Flags().String("memfree", "", "minimum available memory before starting jobs (bytes or K/M/G/T/P suffix; 0 disables)")
	RootCmd.Flags().StringP("out-file", "o", "-", `out file ("-" for stdout)`)

	RootCmd.Flags().StringSliceP("infile", "i", []string{}, "input data file, multi-values supported")

	RootCmd.Flags().StringP("record-delimiter", "D", "\n", `record delimiter (default is "\n")`)
	RootCmd.Flags().StringP("records-join-sep", "J", "\n", `record separator for joining multi-records (default is "\n")`)
	RootCmd.Flags().IntP("nrecords", "n", 1, "number of records sent to a command")
	RootCmd.Flags().StringP("field-delimiter", "d", `\s+`, "field delimiter in records, support regular expression")
	RootCmd.Flags().Bool("pipe", false, "send each group of records to the command's standard input")

	RootCmd.Flags().IntP("retries", "r", 0, "maximum retries (default 0)")
	RootCmd.Flags().Float64P("retry-interval", "", 0, "retry interval (unit: second, supports fractions like 0.5) (default 0)")
	RootCmd.Flags().BoolP("immediate-output", "I", false, "print output immediately and interleaved, to aid debugging")
	RootCmd.Flags().BoolP("print-retry-output", "", true, "print buffered retry output (final attempt is always printed)")
	RootCmd.Flags().IntP("timeout", "t", 0, "timeout of a command (unit: seconds, 0 for no timeout) (default 0)")

	RootCmd.Flags().BoolP("keep-order", "k", false, "keep output in order of input")
	RootCmd.Flags().BoolP("stop-on-error", "e", false, "stop scheduling and clean up active child processes on first error")
	RootCmd.Flags().BoolP("propagate-exit-status", "", true, "propagate child exit status up to the exit status of rush")
	RootCmd.Flags().StringSliceP("no-stop-exes", "", []string{}, "exe names to exclude from stop signal, example: mspdbsrv.exe; or use all for all exes (default none)")
	RootCmd.Flags().StringSliceP("no-kill-exes", "", []string{}, "exe names to exclude from kill signal, example: mspdbsrv.exe; or use all for all exes (default none)")
	RootCmd.Flags().IntP("cleanup-time", "", 3, "time to allow child processes to clean up between stop / kill signals (unit: seconds, 0 for no time) (default 3)")
	RootCmd.Flags().BoolP("dry-run", "", false, "print command but not run")

	RootCmd.Flags().BoolP("continue", "c", false, `continue jobs.`+
		` NOTES: 1) successful commands are saved in file (given by flag -C/--succ-cmd-file);`+
		` 2) if the file does not exist, rush saves data so we can continue jobs next time;`+
		` 3) if the file exists, rush ignores jobs in it and update the file;`+
		` 4) skipped jobs are silent unless --verbose is used`)
	RootCmd.Flags().StringP("succ-cmd-file", "C", "successful_cmds.rush", `file for saving successful commands`)

	// RootCmd.Flags().IntP("buffer-size", "", 1, "buffer size for output of a command before saving to tmpfile (unit: Mb)")

	RootCmd.Flags().StringSliceP("assign", "v", []string{}, "assign the value val to the variable var (format: var=val, val also supports replacement strings)")
	RootCmd.Flags().StringP("trim", "T", "", `trim white space (" \t\r\n") in input (available values: "l" for left, "r" for right, "lr", "rl", "b" for both side)`)
	// RootCmd.Flags().BoolP("greedy", "g", false, `greedy replacement strings (replace again), useful for preset variable, e.g., rush -v p={:^_1} 'echo {p}'`)

	RootCmd.Flags().BoolP("escape", "q", false, `escape special symbols like $ which you can customize by flag -Q/--escape-symbols`)
	RootCmd.Flags().StringP("escape-symbols", "Q", "$#&`", "symbols to escape")
	// RootCmd.Flags().BoolP("escape-curly-brackets", "B", false, `escape curly brackets "{}" in the, e.g., "text{}" or "attr{href}"`)
	protectMultiValueFlags(RootCmd)

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
  17. send a fixed number of records to each command's standard input
      $ seq 10000 | rush --pipe -n 1000 -j 4 'wc -l'

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

	Jobs          int
	StartDelay    time.Duration
	MaxLoad       float64
	MinFreeMemory uint64
	OutFile       string
	ProcessWait   processWait // One-time process condition before starting the run.

	Infiles []string

	RecordDelimiter      string
	RecordsJoinSeparator string
	NRecords             int
	FieldDelimiter       string
	reFieldDelimiter     *regexp.Regexp
	Pipe                 bool

	Retries          int
	RetryInterval    time.Duration
	ImmediateOutput  bool
	PrintRetryOutput bool
	Timeout          time.Duration

	KeepOrder      bool
	StopOnErr      bool
	NoStopExes     []string
	NoKillExes     []string
	CleanupTime    time.Duration
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

	EscapeCurlyBrackets bool
}

// Match the complete assignment and preserve newlines in a CSV-quoted value.
var reAssign = regexp.MustCompile(`(?s)^\s*([a-zA-Z][A-Za-z0-9_]*)\s*=(.+)$`)

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
		if found := reAssign.FindStringSubmatch(s); found != nil {
			assignMap[found[1]] = found[2]
		} else {
			checkError(fmt.Errorf(`illegal value for flag -v/--assign (format: "var=value", type "rush -h" for more details): %s`, s))
		}
	}
	startDelay, err := parseStartDelay(getFlagNonNegativeFloat64(cmd, "delay"))
	checkError(err)
	retryInterval, err := parseSeconds("retry-interval", getFlagNonNegativeFloat64(cmd, "retry-interval"))
	checkError(err)
	timeout, err := parseWholeSeconds("timeout", getFlagNonNegativeInt(cmd, "timeout"))
	checkError(err)
	cleanupTime, err := parseWholeSeconds("cleanup-time", getFlagNonNegativeInt(cmd, "cleanup-time"))
	checkError(err)
	maxLoad, err := parseMaxLoad(getFlagString(cmd, "load"), runtime.NumCPU())
	checkError(err)
	minFreeMemory, err := parseMemorySize(getFlagString(cmd, "memfree"))
	checkError(err)
	processWait, err := parseProcessWait(
		getFlagString(cmd, "wait-for-process"), getFlagString(cmd, "wait-for-process-exit"),
		getFlagFloat64(cmd, "wait-for-process-duration"), getFlagFloat64(cmd, "wait-for-process-interval"),
		cmd.Flags().Changed("wait-for-process-duration"), cmd.Flags().Changed("wait-for-process-interval"),
		getFlagBool(cmd, "wait-for-process-regexp"),
	)
	checkError(err)
	for _, flag := range []string{"wait-for-process", "wait-for-process-exit"} {
		if cmd.Flags().Changed(flag) && getFlagString(cmd, flag) == "" {
			checkError(fmt.Errorf("--%s requires a nonempty process name", flag))
		}
	}

	return Config{
		Verbose: getFlagBool(cmd, "verbose"),
		Version: getFlagBool(cmd, "version"),
		ETA:     getFlagBool(cmd, "eta"),

		Jobs:          getFlagPositiveInt(cmd, "jobs"),
		StartDelay:    startDelay,
		MaxLoad:       maxLoad,
		MinFreeMemory: minFreeMemory,
		OutFile:       getFlagString(cmd, "out-file"),
		ProcessWait:   processWait,

		Infiles: getFlagStringSlice(cmd, "infile"),

		RecordDelimiter:      getFlagString(cmd, "record-delimiter"),
		RecordsJoinSeparator: getFlagString(cmd, "records-join-sep"),
		NRecords:             getFlagPositiveInt(cmd, "nrecords"),
		FieldDelimiter:       getFlagString(cmd, "field-delimiter"),
		Pipe:                 getFlagBool(cmd, "pipe"),

		Retries:          getFlagNonNegativeInt(cmd, "retries"),
		RetryInterval:    retryInterval,
		ImmediateOutput:  getFlagBool(cmd, "immediate-output"),
		PrintRetryOutput: getFlagBool(cmd, "print-retry-output"),
		Timeout:          timeout,

		KeepOrder:      getFlagBool(cmd, "keep-order"),
		StopOnErr:      getFlagBool(cmd, "stop-on-error"),
		NoStopExes:     getFlagStringSlice(cmd, "no-stop-exes"),
		NoKillExes:     getFlagStringSlice(cmd, "no-kill-exes"),
		CleanupTime:    cleanupTime,
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

		EscapeCurlyBrackets: true, // getFlagBool(cmd, "escape-curly-brackets"),
	}
}
