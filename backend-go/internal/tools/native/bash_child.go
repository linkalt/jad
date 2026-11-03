package native

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync/atomic"
	"time"
)

const (
	// Used when a request leaves killDeadline or drainAfterExit at zero (glob).
	bashDrainAfterExit = 500 * time.Millisecond
	bashKillDeadline   = 2 * time.Second
	bashReadChunkSize  = 32 * 1024
)

// Seams for tests: the production values create real pipes and real interpreter processes.
var (
	newBashPipe    = os.Pipe
	newBashCommand = newInterpreterCommand
)

type bashSpillOutcome struct {
	path         string
	writtenBytes int64
	capped       bool
	err          string
}

type bashSpillSink interface {
	write([]byte)
	close() bashSpillOutcome
}

type bashChildRequest struct {
	file        string
	args        []string
	cwd         string
	environment []string
	timeout     time.Duration
	killGrace   time.Duration
	// killDeadline is how long a call waits for the child after SIGKILL; drainAfterExit is
	// how long the output pipes may stay open after the child exited. Zero takes the default.
	killDeadline             time.Duration
	drainAfterExit           time.Duration
	killProcessGroupWhenDone bool
	// sweepAtRunEnd keeps the process group registered with the run (see ChildGroups) while it is alive.
	sweepAtRunEnd bool
	collector     *bashOutputCollector
	spill         map[bashStream]bashSpillSink
	// stdin is written to the child's standard input, which is then closed.
	// Empty leaves the input closed from the start (/dev/null).
	stdin string
}

type bashChildResult struct {
	output      bashRenderedOutput
	exitCode    int
	hasExitCode bool
	signal      string
	timedOut    bool
	aborted     bool
	durationMs  int64
	termination string
	leftRunning bool
	spawnError  error
	spill       map[bashStream]bashSpillOutcome
}

func runBashCommand(ctx context.Context, request bashChildRequest) bashChildResult {
	started := time.Now()
	result := bashChildResult{spill: make(map[bashStream]bashSpillOutcome)}
	if request.collector == nil {
		request.collector = newBashOutputCollector(bashStreamLimits{}, bashStreamLimits{}, 0, true)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if request.killDeadline <= 0 {
		request.killDeadline = bashKillDeadline
	}
	if request.drainAfterExit <= 0 {
		request.drainAfterExit = bashDrainAfterExit
	}
	if ctx.Err() != nil {
		result.aborted = true
		request.collector.end()
		result.output = request.collector.render()
		result.spill = closeBashSpills(request.spill)
		result.durationMs = time.Since(started).Milliseconds()
		return result
	}

	stdoutReader, stdoutWriter, err := newBashPipe()
	if err != nil {
		return finishBashSpawnFailure(result, request.collector, request.spill, err, started)
	}
	stderrReader, stderrWriter, err := newBashPipe()
	if err != nil {
		_ = stdoutReader.Close()
		_ = stdoutWriter.Close()
		return finishBashSpawnFailure(result, request.collector, request.spill, err, started)
	}
	resolvedFile, err := resolveBashExecutable(request.file, request.environment, request.cwd)
	if err != nil {
		_ = stdoutReader.Close()
		_ = stdoutWriter.Close()
		_ = stderrReader.Close()
		_ = stderrWriter.Close()
		return finishBashSpawnFailure(result, request.collector, request.spill, err, started)
	}
	command := newBashCommand(request.file, resolvedFile, request.args...)
	command.Dir = request.cwd
	command.Env = request.environment
	command.Stdout = stdoutWriter
	command.Stderr = stderrWriter
	// An own pipe, not exec's copier: Wait must never block on a descendant that
	// keeps the read end open and never reads it.
	var stdinReader, stdinWriter *os.File
	if request.stdin != "" {
		reader, writer, err := newBashPipe()
		if err != nil {
			_ = stdoutReader.Close()
			_ = stdoutWriter.Close()
			_ = stderrReader.Close()
			_ = stderrWriter.Close()
			return finishBashSpawnFailure(result, request.collector, request.spill, err, started)
		}
		command.Stdin = reader
		stdinReader, stdinWriter = reader, writer
		// Closing the write end when the call ends frees the writer goroutine even
		// if a descendant that holds the read end never reads it.
		defer stdinWriter.Close()
	}
	configureProcessGroup(command)
	if err := command.Start(); err != nil {
		_ = stdoutReader.Close()
		_ = stdoutWriter.Close()
		_ = stderrReader.Close()
		_ = stderrWriter.Close()
		if stdinReader != nil {
			_ = stdinReader.Close()
		}
		return finishBashSpawnFailure(result, request.collector, request.spill, err, started)
	}
	_ = stdoutWriter.Close()
	_ = stderrWriter.Close()
	if stdinReader != nil {
		// The child owns the read end now. A program that exits or never reads its
		// input makes the write fail (EPIPE); that is its choice, not a failed call.
		_ = stdinReader.Close()
		go func(writer *os.File, text string) {
			_, _ = io.WriteString(writer, text)
			_ = writer.Close()
		}(stdinWriter, request.stdin)
	}

	child := &startedBashChild{command: command, stdout: stdoutReader, stderr: stderrReader}
	child.watch()
	return superviseBashChild(ctx, request, child, started, result)
}

// startedBashChild is a started child whose output pipes have no reader yet: one this call
// just started, or a standby interpreter that waited for the call.
type startedBashChild struct {
	command        *exec.Cmd
	stdout, stderr *os.File
	// done is closed once Wait returned; waitErr is its result.
	done    chan struct{}
	waitErr error
}

// watch waits for the child in the background, so its exit is seen even before a call takes it.
func (c *startedBashChild) watch() {
	c.done = make(chan struct{})
	go func() {
		c.waitErr = c.command.Wait()
		close(c.done)
	}()
}

// exited reports whether the child has already ended.
func (c *startedBashChild) exited() bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

// superviseBashChild reads the child's output into the request's collector and spills and
// enforces the request's timeout, cancel, kill and drain rules until the child and its output
// are done. started is when the call began, for the duration.
func superviseBashChild(ctx context.Context, request bashChildRequest, child *startedBashChild, started time.Time, result bashChildResult) bashChildResult {
	command, stdoutReader, stderrReader := child.command, child.stdout, child.stderr
	// Both readers are started below, after the command started, so the count is always 2.
	var readersLeft atomic.Int32
	readersLeft.Store(2)
	readersDone := make(chan struct{})
	go readBashPipe(stdoutReader, bashStdout, request, &readersLeft, readersDone)
	go readBashPipe(stderrReader, bashStderr, request, &readersLeft, readersDone)
	if groups := childGroupsFrom(ctx); groups != nil && request.sweepAtRunEnd {
		pid := command.Process.Pid
		defer groups.add(pid, request.killGrace)
	}

	var waitChannel <-chan struct{} = child.done
	var readersChannel <-chan struct{} = readersDone
	var timeoutTimer *time.Timer
	var timeoutChannel <-chan time.Time
	if request.timeout > 0 {
		timeoutTimer = time.NewTimer(request.timeout)
		timeoutChannel = timeoutTimer.C
	}
	contextChannel := ctx.Done()
	var graceTimer *time.Timer
	var graceChannel <-chan time.Time
	var killTimer *time.Timer
	var killChannel <-chan time.Time
	var drainTimer *time.Timer
	var drainChannel <-chan time.Time
	waitDone, readersFinished := false, false
	var waitErr error
	cleanupStarted := false
	forcedFinish := false
	var cleanupKillTimer *time.Timer

	startDrain := func() {
		if !readersFinished && drainTimer == nil {
			drainTimer = time.NewTimer(request.drainAfterExit)
			drainChannel = drainTimer.C
		}
	}
	startTermination := func(timeout, abort bool) {
		if result.timedOut || result.aborted || waitDone {
			return
		}
		result.timedOut = timeout
		result.aborted = abort
		result.termination = "sigterm"
		signalProcessGroup(command, false)
		graceTimer = time.NewTimer(request.killGrace)
		graceChannel = graceTimer.C
	}
	startNormalCleanup := func() {
		if cleanupStarted {
			return
		}
		cleanupStarted = true
		if request.killProcessGroupWhenDone && !result.timedOut && !result.aborted {
			signalProcessGroup(command, false)
			cleanupKillTimer = killGroupAfter(command, request.killGrace)
		}
		startDrain()
	}

	for !waitDone || !readersFinished {
		select {
		case <-waitChannel:
			waitErr = child.waitErr
			waitDone = true
			waitChannel = nil
			result.exitCode, result.hasExitCode, result.signal = bashExitStatus(waitErr)
			startNormalCleanup()
		case <-readersChannel:
			readersFinished = true
			readersChannel = nil
		case <-timeoutChannel:
			timeoutChannel = nil
			startTermination(true, false)
		case <-contextChannel:
			contextChannel = nil
			startTermination(false, true)
		case <-graceChannel:
			graceChannel = nil
			result.termination = "sigkill"
			signalProcessGroup(command, true)
			killTimer = time.NewTimer(request.killDeadline)
			killChannel = killTimer.C
		case <-killChannel:
			killChannel = nil
			if !waitDone || !readersFinished {
				result.leftRunning = true
				forcedFinish = true
			}
		case <-drainChannel:
			drainChannel = nil
			if !readersFinished {
				forcedFinish = true
			}
		}
		if forcedFinish {
			break
		}
	}
	if timeoutTimer != nil {
		timeoutTimer.Stop()
	}
	if graceTimer != nil {
		graceTimer.Stop()
	}
	if killTimer != nil {
		killTimer.Stop()
	}
	if drainTimer != nil {
		drainTimer.Stop()
	}
	cancelKillIfGroupGone(cleanupKillTimer, command)
	if forcedFinish {
		_ = stdoutReader.Close()
		_ = stderrReader.Close()
		<-readersDone
	}
	_ = stdoutReader.Close()
	_ = stderrReader.Close()
	request.collector.end()
	result.output = request.collector.render()
	result.spill = closeBashSpills(request.spill)
	result.durationMs = time.Since(started).Milliseconds()
	return result
}

// The last reader to exit closes readersDone; the counter is what makes that happen once.
func readBashPipe(reader *os.File, stream bashStream, request bashChildRequest, readersLeft *atomic.Int32, readersDone chan struct{}) {
	defer func() {
		if readersLeft.Add(-1) == 0 {
			close(readersDone)
		}
	}()
	defer reader.Close()
	buffer := make([]byte, bashReadChunkSize)
	for {
		count, err := reader.Read(buffer)
		if count > 0 {
			chunk := buffer[:count]
			request.collector.push(stream, chunk)
			if sink := request.spill[stream]; sink != nil {
				sink.write(chunk)
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				// The tool keeps partial output and reports spill failures separately.
			}
			return
		}
	}
}

func finishBashSpawnFailure(result bashChildResult, collector *bashOutputCollector, spills map[bashStream]bashSpillSink, err error, started time.Time) bashChildResult {
	collector.end()
	result.output = collector.render()
	result.spill = closeBashSpills(spills)
	result.durationMs = time.Since(started).Milliseconds()
	result.spawnError = err
	return result
}

func closeBashSpills(spills map[bashStream]bashSpillSink) map[bashStream]bashSpillOutcome {
	outcomes := make(map[bashStream]bashSpillOutcome)
	for stream, sink := range spills {
		if sink == nil {
			continue
		}
		outcomes[stream] = sink.close()
	}
	return outcomes
}
