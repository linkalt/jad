package native

import (
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"time"
)

const (
	// Used when a request leaves killDeadline or drainAfterExit at zero (glob).
	bashDrainAfterExit = 500 * time.Millisecond
	bashKillDeadline   = 2 * time.Second
	bashReadChunkSize  = 32 * 1024
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

	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		return finishBashSpawnFailure(result, request.collector, request.spill, err, started)
	}
	stderrReader, stderrWriter, err := os.Pipe()
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
	command := newInterpreterCommand(request.file, resolvedFile, request.args...)
	command.Dir = request.cwd
	command.Env = request.environment
	command.Stdout = stdoutWriter
	command.Stderr = stderrWriter
	// An own pipe, not exec's copier: Wait must never block on a descendant that
	// keeps the read end open and never reads it.
	var stdinReader, stdinWriter *os.File
	if request.stdin != "" {
		reader, writer, err := os.Pipe()
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

	var readers sync.WaitGroup
	readers.Add(2)
	go readBashPipe(stdoutReader, bashStdout, request, &readers)
	go readBashPipe(stderrReader, bashStderr, request, &readers)
	readersDone := make(chan struct{})
	go func() {
		readers.Wait()
		close(readersDone)
	}()
	if groups := childGroupsFrom(ctx); groups != nil && request.sweepAtRunEnd {
		pid := command.Process.Pid
		defer groups.add(pid, request.killGrace)
	}
	waitResult := make(chan error, 1)
	go func() { waitResult <- command.Wait() }()

	var waitChannel <-chan error = waitResult
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
		case waitErr = <-waitChannel:
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

func readBashPipe(reader *os.File, stream bashStream, request bashChildRequest, readers *sync.WaitGroup) {
	defer readers.Done()
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
