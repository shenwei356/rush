package process

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	pb "github.com/schollz/progressbar/v3"
	"github.com/shenwei356/go-logging"
	"github.com/shenwei356/rush/internal/runstate"
)

var Log *logging.Logger
var Verbose bool
var TmpOutputDataBuffer = 1 << 20
var OutputChunkSize = 16 << 10
var tmpfilePrefix = fmt.Sprintf("rush.%d.", os.Getpid())
var createSpillFile = func() (*os.File, error) { return os.CreateTemp("", tmpfilePrefix) }
var flushSpillFile = func(w *bufio.Writer) error { return w.Flush() }
var seekSpillFile = func(f *os.File, offset int64, whence int) (int64, error) {
	return f.Seek(offset, whence)
}
var closeSpillFile = func(f *os.File) error { return f.Close() }
var removeSpillFile = os.Remove
var writeSpillFile = func(w *bufio.Writer, p []byte) (int, error) { return w.Write(p) }
var wrapSpillReader = func(r io.Reader) io.Reader { return r }

func init() {
	if Log == nil {
		format := logging.MustStringFormatter(`%{color}[%{level:.4s}]%{color:reset} %{message}`)
		logging.SetBackend(logging.NewBackendFormatter(logging.NewLogBackend(os.Stderr, "", 0), format))
		Log = logging.MustGetLogger("process")
	}
}

var ErrTimeout = errors.New("time out")
var ErrCancelled = errors.New("cancelled")
var ErrMemoryPressure = errors.New("stopped for low memory")

type Command struct {
	ID               uint64
	Cmd              string
	recordCmd        string
	stdin            string
	Cancel           <-chan struct{}
	Timeout          time.Duration
	ctx              context.Context
	ctxCancel        context.CancelFunc
	Ch               chan string
	reader           *bufio.Reader
	tmpfile          string
	tmpfh            *os.File
	finishSendOutput bool
	outputDone       <-chan error
	outputErr        error
	Err              error
	Duration         time.Duration
	dryrun           bool
	exitStatus       int
	Executed         chan int
	controller       processController
	memoryStop       chan struct{}
	memoryStopped    bool // guarded by startGate.activeMu
}

func NewCommand(id uint64, cmdStr string, cancel <-chan struct{}, timeout time.Duration) *Command {
	cmdStr = strings.TrimLeft(cmdStr, " \t\r\n")
	return &Command{ID: id, Cmd: cmdStr, recordCmd: cmdStr, Cancel: cancel, Timeout: timeout, Executed: make(chan int, 2)}
}
func (c *Command) String() string { return fmt.Sprintf("cmd #%d: %s", c.ID, c.Cmd) }

func (c *Command) stopForMemory() bool {
	if c.memoryStop == nil || c.memoryStopped {
		return false
	}
	c.memoryStopped = true
	close(c.memoryStop)
	return true
}

func (c *Command) Run(opts *Options, tryNumber int) (chan string, error) {
	ch := make(chan string, 1)
	outputDone := make(chan error, 1)
	c.outputDone = outputDone
	completeOutput := func(err error) {
		outputDone <- err
		close(outputDone)
	}
	if c.dryrun {
		ch <- c.Cmd + "\n"
		close(ch)
		completeOutput(nil)
		c.Executed <- 1
		close(c.Executed)
		return ch, nil
	}
	started := time.Now()
	defer func() { c.Duration = time.Since(started) }()
	if Verbose {
		Log.Infof("start cmd #%d: %s", c.ID, c.Cmd)
		defer Log.Infof("finish cmd #%d: %s", c.ID, c.Cmd)
	}
	controller := c.controller
	owned := false
	if controller == nil {
		var err error
		controller, err = makeProcessController(opts)
		if err != nil {
			close(ch)
			completeOutput(nil)
			return ch, err
		}
		owned = true
		defer controller.Close()
	}
	command := getCommand(context.Background(), c.Cmd)
	command.Env = append(os.Environ(), "RUSH_CHILD_GROUP=[rush]")
	if c.stdin != "" {
		command.Stdin = strings.NewReader(c.stdin)
	}
	spill := newSpillWriter(TmpOutputDataBuffer)
	var stdout, stderr *lockedWriter
	if opts.ImmediateOutput {
		stdout = &lockedWriter{mu: &opts.ImmediateLock, dst: opts.OutFileHandle}
		stderr = &lockedWriter{mu: &opts.ImmediateLock, dst: opts.ErrFileHandle}
		command.Stdout, command.Stderr = stdout, stderr
	} else {
		stderr = &lockedWriter{mu: &opts.ImmediateLock, dst: opts.ErrFileHandle}
		command.Stdout, command.Stderr = spill, stderr
	}
	var startErr error
	if opts.gate != nil {
		if opts.MinFreeMemory > 0 {
			c.memoryStop = make(chan struct{})
		}
		startErr = opts.gate.start(c, command, controller)
	} else {
		startErr = command.Start()
		if startErr == nil {
			startErr = controller.Started(command)
			if startErr != nil {
				_ = command.Process.Kill()
				_ = command.Wait()
			}
		}
	}
	if startErr != nil {
		close(ch)
		completeOutput(nil)
		close(c.Executed)
		c.exitStatus = 1
		c.Err = fmt.Errorf("start cmd #%d: %w", c.ID, startErr)
		return ch, c.Err
	}
	defer controller.Finished(command)
	if opts.gate != nil {
		defer opts.gate.finished(c)
	}
	waited := make(chan error, 1)
	go func() { waited <- command.Wait() }()
	var timeout <-chan time.Time
	var timer *time.Timer
	if c.Timeout > 0 {
		timer = time.NewTimer(c.Timeout)
		timeout = timer.C
		defer timer.Stop()
	}
	var waitErr, terminal error
	select {
	case waitErr = <-waited:
	case <-c.Cancel:
		terminal = ErrCancelled
		_ = controller.KillCommand(command)
		waitErr = <-waited
	case <-c.memoryStop:
		terminal = ErrMemoryPressure
		if isClosed(c.Cancel) {
			terminal = ErrCancelled
		}
		_ = controller.KillCommand(command)
		waitErr = <-waited
	case <-timeout:
		terminal = ErrTimeout
		_ = controller.KillCommand(command)
		waitErr = <-waited
	}
	c.exitStatus = command.ProcessState.ExitCode()
	if errors.Is(terminal, ErrTimeout) {
		c.exitStatus = 124
	}
	if errors.Is(terminal, ErrCancelled) && c.exitStatus == 0 {
		c.exitStatus = 1
	}
	// A nonzero child status is already an externally visible terminal cause.
	// Keep it primary while carrying any independent output failure separately.
	if terminal == nil && waitErr != nil && c.exitStatus != 0 {
		terminal = fmt.Errorf("wait cmd #%d: %s: %w", c.ID, c.Cmd, waitErr)
	}
	if opts.ImmediateOutput {
		c.outputErr = firstWriterError(stdout, stderr)
		if c.outputErr != nil && terminal == nil {
			terminal = outputFailure{c.outputErr}
		}
		close(ch)
		completeOutput(c.outputErr)
		c.finishSendOutput = true
	} else {
		reader, spillErr := spill.finish()
		c.outputErr = combineErrors(spillErr, firstWriterError(stderr))
		if c.outputErr != nil && terminal == nil {
			terminal = outputFailure{c.outputErr}
		}
		c.tmpfh, c.tmpfile = spill.file, spill.name
		if reader != nil {
			c.reader = bufio.NewReader(reader)
		}
		go c.sendOutput(ch, outputDone, c.outputErr)
	}
	if terminal == nil && waitErr != nil {
		terminal = fmt.Errorf("wait cmd #%d: %s: %w", c.ID, c.Cmd, waitErr)
	}
	if terminal != nil && c.exitStatus == 0 {
		c.exitStatus = 1
	}
	if terminal == nil {
		c.Executed <- 1
	}
	close(c.Executed)
	c.Err = terminal
	if owned && terminal != nil {
		_ = controller.StopAll(opts.CleanupTime, opts.forceStopChannel())
	}
	return ch, terminal
}

func (c *Command) sendOutput(ch chan string, done chan<- error, initialErr error) {
	defer close(ch)
	defer func() { c.finishSendOutput = true }()
	if c.reader == nil {
		done <- initialErr
		close(done)
		return
	}
	var readErr error
	defer func() {
		done <- combineErrors(initialErr, readErr)
		close(done)
	}()
	buf := make([]byte, OutputChunkSize)
	for {
		n, err := c.reader.Read(buf)
		if n > 0 {
			ch <- string(buf[:n])
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				readErr = fmt.Errorf("read buffered output for cmd #%d: %w", c.ID, err)
			}
			return
		}
	}
}

func combineErrors(errs ...error) error {
	filtered := make([]error, 0, len(errs))
	for _, err := range errs {
		if err != nil {
			filtered = append(filtered, err)
		}
	}
	if len(filtered) == 0 {
		return nil
	}
	if len(filtered) == 1 {
		return filtered[0]
	}
	return errors.Join(filtered...)
}

type outputFailure struct{ error }

func isOutputFailure(err error) bool {
	var failure outputFailure
	return errors.As(err, &failure)
}

func (c *Command) Cleanup() error {
	var first error
	if c.tmpfh != nil {
		if err := closeSpillFile(c.tmpfh); err != nil {
			first = err
		}
		c.tmpfh = nil
	}
	if c.tmpfile != "" {
		if err := removeSpillFile(c.tmpfile); err != nil && !errors.Is(err, os.ErrNotExist) && first == nil {
			first = err
		}
		c.tmpfile = ""
	}
	return first
}
func (c *Command) getExitStatus(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return 1
}

type spillWriter struct {
	limit int
	mem   bytes.Buffer
	file  *os.File
	buf   *bufio.Writer
	name  string
	err   error
}

func newSpillWriter(limit int) *spillWriter { return &spillWriter{limit: limit} }
func (w *spillWriter) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	if w.file == nil && w.mem.Len()+len(p) <= w.limit {
		return w.mem.Write(p)
	}
	if w.file == nil {
		f, err := createSpillFile()
		if err != nil {
			w.err = err
			return 0, err
		}
		w.file, w.name, w.buf = f, f.Name(), bufio.NewWriter(f)
		if err := writeAllWith(writeSpillFile, w.buf, w.mem.Bytes()); err != nil {
			w.err = err
			return 0, err
		}
		w.mem.Reset()
	}
	n, err := writeSpillFile(w.buf, p)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	if err != nil {
		w.err = err
	}
	return n, err
}
func (w *spillWriter) finish() (io.Reader, error) {
	if w.err != nil {
		return nil, w.err
	}
	if w.file == nil {
		return wrapSpillReader(bytes.NewReader(w.mem.Bytes())), nil
	}
	if err := flushSpillFile(w.buf); err != nil {
		w.err = err
		return nil, err
	}
	if _, err := seekSpillFile(w.file, 0, io.SeekStart); err != nil {
		w.err = err
		return nil, err
	}
	return wrapSpillReader(w.file), nil
}
func writeAllWith(write func(*bufio.Writer, []byte) (int, error), dst *bufio.Writer, p []byte) error {
	n, err := write(dst, p)
	if err != nil {
		return err
	}
	if n != len(p) {
		return io.ErrShortWrite
	}
	return nil
}
func writeAll(dst io.Writer, p []byte) error {
	n, err := dst.Write(p)
	if err != nil {
		return err
	}
	if n != len(p) {
		return io.ErrShortWrite
	}
	return nil
}

type lockedWriter struct {
	mu    *sync.Mutex
	dst   io.Writer
	errMu sync.Mutex
	err   error
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	if w == nil || w.dst == nil {
		return len(p), nil
	}
	w.mu.Lock()
	n, err := w.dst.Write(p)
	w.mu.Unlock()
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	if err != nil {
		w.errMu.Lock()
		if w.err == nil {
			w.err = err
		}
		w.errMu.Unlock()
	}
	return n, err
}
func (w *lockedWriter) Error() error {
	if w == nil {
		return nil
	}
	w.errMu.Lock()
	defer w.errMu.Unlock()
	return w.err
}
func firstWriterError(ws ...*lockedWriter) error {
	for _, w := range ws {
		if err := w.Error(); err != nil {
			return err
		}
	}
	return nil
}

type ImmediateLineWriter struct {
	lock      *sync.Mutex
	numJobs   int
	cmdId     uint64
	tryNumber int
}

func NewImmediateLineWriter(lock *sync.Mutex, numJobs int, cmdId uint64, tryNumber int) *ImmediateLineWriter {
	return &ImmediateLineWriter{lock: lock, numJobs: numJobs, cmdId: cmdId, tryNumber: tryNumber}
}
func (lw *ImmediateLineWriter) WritePrefixedLines(input string, fh *os.File) {
	if fh == nil {
		return
	}
	if lw.lock != nil {
		lw.lock.Lock()
		defer lw.lock.Unlock()
	}
	_, _ = fh.WriteString(input)
}

type ImmediateWriter struct {
	lineWriter *ImmediateLineWriter
	fh         *os.File
}

func NewImmediateWriter(lw *ImmediateLineWriter, fh *os.File) *ImmediateWriter {
	return &ImmediateWriter{lineWriter: lw, fh: fh}
}
func (iw ImmediateWriter) Write(p []byte) (int, error) {
	if iw.fh == nil {
		return len(p), nil
	}
	if iw.lineWriter != nil && iw.lineWriter.lock != nil {
		iw.lineWriter.lock.Lock()
		defer iw.lineWriter.lock.Unlock()
	}
	n, err := iw.fh.Write(p)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	return n, err
}

type IntSet struct{ set sync.Map }

func (s *IntSet) Add(i int) bool { _, loaded := s.set.LoadOrStore(i, true); return !loaded }

type TopLevelEnum int

const (
	NotTopLevel TopLevelEnum = iota
	TopLevel
)

func lexEncode(n uint64, _ TopLevelEnum) string { return fmt.Sprintf("%d", n) }
func getEntrySeparator() string                 { return "/" }
func includeImmediatePrefix(id uint64, try int, line uint64, data *string) {
	if data != nil {
		*data += fmt.Sprintf("(%d/%d/%d): ", id, try, line)
	}
}

const (
	INVALID_HANDLE = 0
	CTRL_C_SIGNAL  = iota
	CTRL_BREAK_SIGNAL
	KILL_SIGNAL
)
const (
	SEND_NO_SIGNAL     = 0
	SEND_CTRL_C_SIGNAL = 1 << iota
	SEND_CTRL_BREAK_SIGNAL
	SEND_KILL_SIGNAL
)

func canSendSignal(name string, excluded []string) (bool, error) {
	for _, x := range excluded {
		if x == "all" || strings.EqualFold(name, x) {
			return false, nil
		}
	}
	return true, nil
}
func getSignalsToSend(name string, noStop, noKill []string) (int, error) {
	stop, _ := canSendSignal(name, noStop)
	kill, _ := canSendSignal(name, noKill)
	n := 0
	if stop {
		n |= SEND_CTRL_C_SIGNAL | SEND_CTRL_BREAK_SIGNAL
	}
	if kill {
		n |= SEND_KILL_SIGNAL
	}
	return n, nil
}

type ProcessRecord struct {
	pid           int
	processHandle int
	processExists bool
	accessGranted bool
	signalsToSend int
	pgid          int
	identity      uint64
	name          string
}

type processController interface {
	Started(*exec.Cmd) error
	Finished(*exec.Cmd)
	KillCommand(*exec.Cmd) error
	StopAll(time.Duration, <-chan struct{}) error
	Close() error
}

var makeProcessController = newPlatformProcessController

type Options struct {
	DryRun              bool
	Jobs                int
	ETA                 bool
	ETABar              *pb.ProgressBar
	KeepOrder           bool
	Retries             int
	RetryInterval       time.Duration
	OutFileHandle       *os.File
	ErrFileHandle       *os.File
	ImmediateOutput     bool
	ImmediateLock       sync.Mutex
	PrintRetryOutput    bool
	Timeout             time.Duration
	StopOnErr           bool
	PidRecordsLock      sync.Mutex
	NoStopExes          []string
	NoKillExes          []string
	CleanupTime         time.Duration
	StartDelay          time.Duration
	MaxLoad             float64
	MinFreeMemory       uint64
	PropExitStatus      bool
	RecordSuccessfulCmd bool
	Verbose             bool
	stopOnce            sync.Once
	stopOnErrorOnce     sync.Once
	forceStopInit       sync.Once
	forceStopOnce       sync.Once
	forceStop           chan struct{}
	controller          processController
	gate                *startGate
	state               *runstate.State
}

func (o *Options) forceStopChannel() <-chan struct{} {
	if o.state != nil {
		return o.state.ForceChan()
	}
	o.forceStopInit.Do(func() { o.forceStop = make(chan struct{}) })
	return o.forceStop
}
func (o *Options) ForceStop() {
	if o.state != nil {
		o.state.Force()
		return
	}
	o.forceStopOnce.Do(func() {
		o.forceStopChannel()
		close(o.forceStop)
	})
}
func (o *Options) stopChildren() {
	o.stopOnce.Do(func() {
		if o.controller != nil {
			_ = o.controller.StopAll(o.CleanupTime, o.forceStopChannel())
		}
	})
}

// Job keeps the shell command separate from the text recorded by
// resume/continue features.
type Job struct {
	Cmd       string // Cmd is the command text passed to the shell.
	RecordCmd string // RecordCmd is the stable text recorded after success.
	Stdin     string // Stdin is replayed for every execution attempt.
}

type commandInput interface {
	string | Job
}

type runResult struct {
	command   *Command
	success   bool
	status    int
	outputErr error
	retry     *runTask
}

type runTask struct {
	id                  uint64
	text, record, stdin string
	attempt             int
	parts               []outputPart
}

type outputPart struct {
	command *Command
	ch      <-chan string
	done    <-chan error
	emit    bool
}

func combineCommandOutputs(parts []outputPart) (chan string, <-chan error) {
	out := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		var first error
		for _, part := range parts {
			for value := range part.ch {
				if part.emit {
					out <- value
				}
			}
			if err := <-part.done; err != nil && first == nil {
				first = err
			}
			if err := part.command.Cleanup(); err != nil && first == nil {
				first = err
			}
		}
		close(out)
		done <- first
		close(done)
	}()
	return out, done
}

func Run4Output(opts *Options, cancel chan struct{}, input chan string) (chan string, chan string, chan int, chan int) {
	ctx, stop := contextFromCancel(cancel)
	out, success, done, statuses := Run4OutputContext(opts, ctx, stop, input)
	return out, success, cancelAfterDone(done, stop), statuses
}
func Run(opts *Options, cancel chan struct{}, input chan string) (chan *Command, chan string, chan int, chan int) {
	ctx, stop := contextFromCancel(cancel)
	cmds, success, done, statuses := runContext(opts, ctx, stop, input)
	return cmds, success, cancelAfterDone(done, stop), statuses
}

func Run4OutputContext(opts *Options, ctx context.Context, stop context.CancelFunc, input chan string) (chan string, chan string, chan int, chan int) {
	return run4OutputContext(opts, ctx, stop, input)
}

// Run4OutputContextJobs is like Run4OutputContext, but accepts separate
// successful-command record keys.
func Run4OutputContextJobs(opts *Options, ctx context.Context, stop context.CancelFunc, input chan Job) (chan string, chan string, chan int, chan int) {
	return run4OutputContext(opts, ctx, stop, input)
}

func run4OutputContext[T commandInput](opts *Options, ctx context.Context, stop context.CancelFunc, input chan T) (chan string, chan string, chan int, chan int) {
	commands, success, commandDone, statuses := runContext(opts, ctx, stop, input)
	out := make(chan string, max(1, opts.Jobs))
	done := make(chan int, 1)
	go func() {
		defer func() { close(out); done <- 1; close(done) }()
		next := uint64(1)
		pending := make(map[uint64]*Command)
		emit := func(c *Command) {
			for msg := range c.Ch {
				out <- msg
			}
			if err := c.Cleanup(); err != nil {
				opts.state.Stop(runstate.Cause{Kind: runstate.Internal, Status: 1})
			}
			if opts.ETA {
				// Only increment progress bar for commands that were not cancelled
				if !errors.Is(c.Err, ErrCancelled) {
					_ = opts.ETABar.Add(1)
				}
			}
		}
		for c := range commands {
			if !opts.KeepOrder {
				emit(c)
				continue
			}
			pending[c.ID] = c
			for pending[next] != nil {
				emit(pending[next])
				delete(pending, next)
				next++
			}
		}
		ids := make([]int, 0, len(pending))
		for id := range pending {
			ids = append(ids, int(id))
		}
		sort.Ints(ids)
		for _, id := range ids {
			emit(pending[uint64(id)])
		}
		<-commandDone
	}()
	return out, success, done, statuses
}

func runContext[T commandInput](opts *Options, parent context.Context, stop context.CancelFunc, input chan T) (chan *Command, chan string, chan int, chan int) {
	if opts.Jobs < 1 {
		opts.Jobs = 1
	}
	if opts.OutFileHandle == nil {
		opts.OutFileHandle = os.Stdout
	}
	if opts.ErrFileHandle == nil {
		opts.ErrFileHandle = os.Stderr
	}
	Verbose = opts.Verbose
	state, ok := runstate.FromContext(parent)
	ctx := parent
	if !ok {
		state, ctx = runstate.New(parent)
	}
	opts.state = state
	controller, controllerErr := makeProcessController(opts)
	opts.controller = controller
	opts.gate = nil
	if opts.StartDelay > 0 || opts.MaxLoad > 0 || opts.MinFreeMemory > 0 {
		opts.gate = newStartGate(ctx, opts, state)
	}
	commands := make(chan *Command, opts.Jobs)
	success := make(chan string, opts.Jobs)
	done := make(chan int, 1)
	var statuses chan int
	if opts.PropExitStatus {
		statuses = make(chan int, opts.Jobs)
	}
	results := make(chan runResult, opts.Jobs)
	type outputCompletion struct {
		sequence uint64
		err      error
	}
	type supervisedResult struct {
		result                runResult
		outputComplete        bool
		outputFailureReported bool
	}
	outputCompletions := make(chan outputCompletion, opts.Jobs)
	go func() {
		defer func() { done <- 1; close(done) }()
		if opts.gate != nil {
			defer opts.gate.close()
		}
		if controllerErr != nil {
			state.Stop(runstate.Cause{Kind: runstate.Internal, Status: 1})
			if controller != nil {
				_ = controller.Close()
			}
			close(commands)
			close(success)
			if statuses != nil {
				close(statuses)
			}
			return
		}
		var workers sync.WaitGroup
		var outputWaiters sync.WaitGroup
		id := uint64(1)
		active := 0
		inflight := 0
		var retryQueue []*runTask
		awaitingOutput := 0
		inputOpen := true
		cancelled := false
		cancelCh := ctx.Done()
		nextSequence := uint64(1)
		nextFinalize := uint64(1)
		pending := make(map[uint64]*supervisedResult)
		reportOutputFailure := func(result *supervisedResult, err error) {
			if err == nil {
				return
			}
			result.result.outputErr = err
			result.result.success = false
			if result.result.status == 0 {
				result.result.status = 1
			}
			if result.result.command != nil {
				result.result.command.outputErr = err
				if result.result.command.Err == nil {
					result.result.command.Err = outputFailure{err}
				}
			}
			if !result.outputFailureReported {
				Log.Error(err)
				state.Stop(runstate.Cause{Kind: runstate.Internal, Status: 1})
				result.outputFailureReported = true
			}
		}
		finalizeReady := func() {
			for {
				result := pending[nextFinalize]
				if result == nil || !result.outputComplete {
					return
				}
				if statuses != nil {
					statuses <- result.result.status
				}
				if result.result.success && opts.RecordSuccessfulCmd {
					recordCmd := result.result.command.recordCmd
					if recordCmd == "" {
						recordCmd = result.result.command.Cmd
					}
					success <- recordCmd
				}
				delete(pending, nextFinalize)
				nextFinalize++
			}
		}
		startTask := func(task *runTask) {
			workers.Add(1)
			active++
			inflight++
			go func() {
				defer workers.Done()
				results <- executeWithRetries(ctx, opts, controller, task)
			}()
		}
		for inputOpen || inflight > 0 || len(retryQueue) > 0 {
			if len(retryQueue) > 0 && inflight < opts.Jobs && !cancelled && ctx.Err() == nil {
				task := retryQueue[0]
				retryQueue[0] = nil
				retryQueue = retryQueue[1:]
				startTask(task)
				continue
			}
			if ctx.Err() != nil && len(retryQueue) > 0 {
				for _, task := range retryQueue {
					for _, part := range task.parts {
						for range part.ch {
						}
						<-part.done
						_ = part.command.Cleanup()
					}
				}
				retryQueue = nil
			}
			if !inputOpen && inflight == 0 && len(retryQueue) == 0 {
				break
			}
			var inputCh <-chan T
			if inputOpen && !cancelled && inflight < opts.Jobs && len(retryQueue) == 0 {
				inputCh = input
			}
			var resultCh <-chan runResult
			if active > 0 {
				resultCh = results
			}
			var outputCompletionCh <-chan outputCompletion
			if awaitingOutput > 0 {
				outputCompletionCh = outputCompletions
			}
			select {
			case <-cancelCh:
				cancelled = true
				inputOpen = false
				cancelCh = nil
			case inputValue, open := <-inputCh:
				if !open {
					inputOpen = false
					continue
				}
				text := ""
				recordText := ""
				stdin := ""
				switch value := any(inputValue).(type) {
				case string:
					text = value
					recordText = value
				case Job:
					text = value.Cmd
					recordText = value.RecordCmd
					stdin = value.Stdin
					if recordText == "" {
						recordText = text
					}
				}
				// A ready input send and cancellation may race in select. Recheck
				// before launching so queued work never starts after cancellation.
				if ctx.Err() != nil {
					cancelled = true
					inputOpen = false
					continue
				}
				startTask(&runTask{id: id, text: text, record: recordText, stdin: stdin})
				id++
			case result := <-resultCh:
				active--
				if result.retry != nil {
					inflight--
					retryQueue = append(retryQueue, result.retry)
					continue
				}
				sequence := nextSequence
				nextSequence++
				supervised := &supervisedResult{result: result}
				pending[sequence] = supervised
				if result.outputErr != nil {
					reportOutputFailure(supervised, result.outputErr)
				}
				if result.command == nil {
					supervised.outputComplete = true
					inflight--
					finalizeReady()
					continue
				}
				commands <- result.command
				awaitingOutput++
				outputWaiters.Add(1)
				go func(sequence uint64, outputDone <-chan error) {
					defer outputWaiters.Done()
					outputCompletions <- outputCompletion{sequence: sequence, err: <-outputDone}
				}(sequence, result.command.outputDone)
			case completion := <-outputCompletionCh:
				awaitingOutput--
				inflight--
				supervised := pending[completion.sequence]
				if supervised != nil {
					reportOutputFailure(supervised, completion.err)
					supervised.outputComplete = true
				}
				finalizeReady()
			}
		}
		workers.Wait()
		outputWaiters.Wait()
		if ctx.Err() != nil {
			if err := controller.StopAll(opts.CleanupTime, state.ForceChan()); err != nil {
				state.Record(runstate.Cause{Kind: runstate.Internal, Status: 1})
			}
		}
		if err := controller.Close(); err != nil {
			state.Record(runstate.Cause{Kind: runstate.Internal, Status: 1})
		}
		close(commands)
		close(success)
		if statuses != nil {
			close(statuses)
		}
	}()
	return commands, success, done, statuses
}

func executeWithRetries(ctx context.Context, opts *Options, controller processController, task *runTask) runResult {
	var command *Command
	finish := func(success bool, status int, outputErr error) runResult {
		command.Ch, command.outputDone = combineCommandOutputs(task.parts)
		return runResult{command: command, success: success, status: status, outputErr: outputErr}
	}
	for attempt := task.attempt; attempt <= opts.Retries; attempt++ {
		command = NewCommand(task.id, task.text, ctx.Done(), opts.Timeout)
		command.recordCmd = task.record
		command.stdin = task.stdin
		command.controller = controller
		command.dryrun = opts.DryRun
		ch, err := command.Run(opts, attempt+1)
		if errors.Is(err, ErrMemoryPressure) && command.outputErr != nil {
			opts.state.Stop(runstate.Cause{Kind: runstate.Internal, Status: 1})
		}
		if errors.Is(err, ErrMemoryPressure) && ctx.Err() == nil && command.outputErr == nil {
			// A memory stop is independent of the user's retry budget. Discard
			// its partial output before putting the same logical job back.
			for range ch {
			}
			if outputErr := <-command.outputDone; outputErr != nil {
				opts.state.Stop(runstate.Cause{Kind: runstate.Internal, Status: 1})
				return finish(false, 1, outputErr)
			}
			if cleanupErr := command.Cleanup(); cleanupErr != nil {
				opts.state.Stop(runstate.Cause{Kind: runstate.Internal, Status: 1})
				return finish(false, 1, cleanupErr)
			}
			task.attempt = attempt
			return runResult{retry: task}
		}
		if command.outputErr != nil {
			if err != nil {
				Log.Error(err)
			}
			task.parts = append(task.parts, outputPart{command: command, ch: ch, done: command.outputDone, emit: opts.PrintRetryOutput || attempt == opts.Retries})
			status := command.exitStatus
			if status == 0 {
				status = 1
			}
			if opts.StopOnErr && err != nil && !isOutputFailure(err) && !errors.Is(err, ErrCancelled) {
				opts.state.Stop(runstate.Cause{Kind: runstate.StopOnError, Status: status})
				Log.Error("stop on first error")
			}
			return finish(false, status, command.outputErr)
		}
		if err == nil {
			task.parts = append(task.parts, outputPart{command: command, ch: ch, done: command.outputDone, emit: true})
			return finish(true, command.exitStatus, nil)
		}
		task.parts = append(task.parts, outputPart{command: command, ch: ch, done: command.outputDone, emit: opts.PrintRetryOutput || attempt == opts.Retries})
		if isOutputFailure(err) {
			Log.Error(err)
			return finish(false, 1, err)
		}
		if opts.StopOnErr && !errors.Is(err, ErrCancelled) {
			Log.Error(err)
			status := command.exitStatus
			if status == 0 {
				status = 1
			}
			opts.state.Stop(runstate.Cause{Kind: runstate.StopOnError, Status: status})
			Log.Error("stop on first error")
			return finish(false, status, nil)
		}
		if errors.Is(err, ErrCancelled) || attempt == opts.Retries {
			Log.Error(err)
			return finish(false, command.exitStatus, nil)
		}
		Log.Warning(err)
		timer := time.NewTimer(opts.RetryInterval)
		select {
		case <-timer.C:
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return finish(false, command.exitStatus, nil)
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}
	return runResult{command: command}
}

func drain(ch <-chan string) {
	for range ch {
	}
}
func drainAndCleanup(c *Command) { drain(c.Ch); _ = c.Cleanup() }
func combine(inputs []<-chan string) chan string {
	out := make(chan string, 1)
	go func() {
		defer close(out)
		for _, input := range inputs {
			for value := range input {
				out <- value
			}
		}
	}()
	return out
}
func combineWorker(input <-chan string, output chan<- string, wg *sync.WaitGroup) {
	defer wg.Done()
	for value := range input {
		output <- value
	}
}
func contextFromCancel(cancel <-chan struct{}) (context.Context, context.CancelFunc) {
	ctx, stop := context.WithCancel(context.Background())
	go func() {
		select {
		case <-cancel:
			stop()
		case <-ctx.Done():
		}
	}()
	return ctx, stop
}
func cancelAfterDone(done <-chan int, stop context.CancelFunc) chan int {
	out := make(chan int, 1)
	go func() {
		value, ok := <-done
		stop()
		if ok {
			out <- value
		}
		close(out)
	}()
	return out
}
func containsMarker(env string) bool {
	return regexp.MustCompile(`RUSH_CHILD_GROUP=.*\[rush\]`).MatchString(env)
}
func getChildMarkerKey() string           { return "RUSH_CHILD_GROUP" }
func getChildMarkerValue() string         { return "[rush]" }
func getChildMarkerRegex() *regexp.Regexp { return regexp.MustCompile(`RUSH_CHILD_GROUP=.*\[rush\]`) }
