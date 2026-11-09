package native

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"jad/backend-go/internal/sessionfs"
)

// A standby is an interpreter exec started before the call that will use it. It waits in its
// shim, which reads the call's code from fd 3 and answers on fd 4: R when it runs the code, F
// when it cannot run it the way a fresh interpreter would. A pipe standby has no shim: a mode
// file runner starts with its usual argv while its code file is a named pipe, and the
// interpreter reads the call's code from it. A gate standby (tool_smith) has fd 3 only: it
// loads what it can before the call and goes on when fd 3 ends, once the call's files are
// written; it does not answer. A call that takes a standby skips the
// interpreter's start-up; each standby still runs one call and ends with it, so a program sees
// no state of another call.
const (
	standbyCodeFD    = 3
	standbyControlFD = 4
	standbyAccept    = 'R'
	// standbyShimName is the shim's file in a mode file runner's directory; the call's code
	// file keeps its own name, so a fall back to a fresh start can use the same directory.
	standbyShimName = "standby"
	// standbyKeyDir stands in for the per-standby directory when the key is computed, so two
	// launches that differ only in their directory share a key.
	standbyKeyDir = "\x00standby-dir\x00"
)

//go:embed exec_standby/python.py exec_standby/js.js exec_standby/ruby.rb
var standbyShimFiles embed.FS

var standbyShimSources = map[string]string{
	sessionfs.NativeExecStandbyPython: "exec_standby/python.py",
	sessionfs.NativeExecStandbyJS:     "exec_standby/js.js",
	sessionfs.NativeExecStandbyRuby:   "exec_standby/ruby.rb",
}

// standbyShim is the shim a runner waits in, or "" when its calls start their own interpreter.
func standbyShim(lang sessionfs.NativeExecLang) string {
	if lang.StandbyCode != "" {
		return lang.StandbyCode
	}
	source, ok := standbyShimSources[lang.Standby]
	if !ok {
		return ""
	}
	text, err := standbyShimFiles.ReadFile(source)
	if err != nil {
		return ""
	}
	return string(text)
}

// standbySpawn is one launch of a waiting interpreter: what runs, where, with what. The key
// is everything that makes two launches the same process, so a call only takes a standby that
// it would have started itself.
type standbySpawn struct {
	file        string
	argv        []string
	cwd         string
	environment []string
	// files are written into the standby's directory before it starts (template, shim file).
	files []standbyFile
	// pipe is the code file of a pipe standby, made a named pipe before it starts.
	pipe string
	// gate gives the standby fd 3 and no fd 4: it waits for fd 3 to end and does not answer.
	gate bool
	// token is a secret in the environment that the call must use (tool_smith's bridge token);
	// it is not part of the key.
	token string
	// sandbox, workspace, tmp and sessionDir start the standby in the sandbox the call would
	// start in.
	sandbox    *sessionfs.SandboxSettings
	workspace  string
	tmp        string
	sessionDir string
}

type standbyFile struct {
	path string
	data []byte
	dir  bool
}

func standbyKey(spawn standbySpawn) string {
	digest := sha256.New()
	field := func(text string) {
		var size [8]byte
		binary.LittleEndian.PutUint64(size[:], uint64(len(text)))
		digest.Write(size[:])
		io.WriteString(digest, text)
	}
	count := func(n int) {
		var size [8]byte
		binary.LittleEndian.PutUint64(size[:], uint64(n))
		digest.Write(size[:])
	}
	field(spawn.file)
	field(spawn.cwd)
	field(spawn.pipe)
	field(sandboxKey(spawn.sandbox, sandboxPlace{workspace: spawn.workspace, cwd: spawn.cwd, tmp: spawn.tmp, sessionDir: spawn.sessionDir}))
	if spawn.gate {
		count(1)
	} else {
		count(0)
	}
	for _, list := range [][]string{spawn.argv, spawn.environment} {
		count(len(list))
		for _, item := range list {
			field(item)
		}
	}
	count(len(spawn.files))
	for _, file := range spawn.files {
		field(file.path)
		field(string(file.data))
		if file.dir {
			count(1)
		} else {
			count(0)
		}
	}
	return string(digest.Sum(nil))
}

// standbyProcess is one waiting interpreter and the parent's ends of its pipes.
type standbyProcess struct {
	key     string
	workDir string
	child   *startedBashChild
	stdin   *os.File
	// code is the write end of fd 3, or of the code file's named pipe (pipe standby); control
	// is the read end of fd 4, nil for a pipe standby, which does not answer.
	code    *os.File
	control *os.File
	timer   *time.Timer
	// token is the secret the standby was started with (standbySpawn.token).
	token string
}

// stop ends a standby no call ran in. Its directory stays: a call that took it owns the
// directory and may still use it (a declined shim falls back to a fresh start in it).
func (p *standbyProcess) stop() {
	if !p.child.exited() {
		signalProcessGroup(p.child.command, true)
	}
	p.closePipes()
	_ = p.child.stdout.Close()
	_ = p.child.stderr.Close()
}

// discard stops a standby the pool still owns and removes its directory once it ended.
func (p *standbyProcess) discard() {
	p.stop()
	go func() {
		<-p.child.done
		_ = os.RemoveAll(p.workDir)
	}()
}

func (p *standbyProcess) closePipes() {
	_ = p.stdin.Close()
	_ = p.code.Close()
	if p.control != nil {
		_ = p.control.Close()
	}
}

// standbyPool holds the waiting interpreters of the whole backend, by launch key. A tool
// catalog is built per run, so the pool belongs to the process, not to one exec tool.
type standbyPool struct {
	mu       sync.Mutex
	idle     map[string][]*standbyProcess
	starting map[string]int
	// count is every waiting or starting standby, against standby_max.
	count int
	// brokenUntil holds back a key whose standby failed to start or died waiting.
	brokenUntil map[string]time.Time
	now         func() time.Time
	start       func(standbySpawn) (*standbyProcess, error)
	// taken counts the standbys handed to calls.
	taken int
}

var execStandby = newStandbyPool()

func newStandbyPool() *standbyPool {
	return &standbyPool{
		idle:        map[string][]*standbyProcess{},
		starting:    map[string]int{},
		brokenUntil: map[string]time.Time{},
		now:         time.Now,
		start:       startStandby,
	}
}

// take hands out the oldest live standby of key, or nil. One that died while waiting is
// dropped and holds its key back like a failed start.
func (pool *standbyPool) take(key string, idle time.Duration) *standbyProcess {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	for len(pool.idle[key]) > 0 {
		process := pool.idle[key][0]
		pool.idle[key] = pool.idle[key][1:]
		if len(pool.idle[key]) == 0 {
			delete(pool.idle, key)
		}
		pool.count--
		process.timer.Stop()
		if process.child.exited() {
			pool.brokenUntil[key] = pool.now().Add(idle)
			process.discard()
			continue
		}
		// The scratch sweep goes by age; a standby that waited is as new as its call.
		now := time.Now()
		_ = os.Chtimes(process.workDir, now, now)
		pool.taken++
		return process
	}
	return nil
}

// refill starts standbys for key in the background until want wait or start, unless the
// backend already holds max or the key is held back.
func (pool *standbyPool) refill(key string, makeDir func() (string, error), spawn func(workDir string) (standbySpawn, error), want, max int, idle time.Duration) {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if until, broken := pool.brokenUntil[key]; broken {
		if pool.now().Before(until) {
			return
		}
		delete(pool.brokenUntil, key)
	}
	for len(pool.idle[key])+pool.starting[key] < want && pool.count < max {
		pool.starting[key]++
		pool.count++
		go pool.launch(key, makeDir, spawn, idle)
	}
}

func (pool *standbyPool) launch(key string, makeDir func() (string, error), spawn func(workDir string) (standbySpawn, error), idle time.Duration) {
	process, err := pool.startIn(makeDir, spawn)
	pool.mu.Lock()
	defer pool.mu.Unlock()
	pool.starting[key]--
	if pool.starting[key] == 0 {
		delete(pool.starting, key)
	}
	if err != nil {
		pool.count--
		pool.brokenUntil[key] = pool.now().Add(idle)
		return
	}
	process.key = key
	process.timer = time.AfterFunc(idle, func() { pool.expire(process) })
	pool.idle[key] = append(pool.idle[key], process)
	go func() {
		<-process.child.done
		pool.died(process, idle)
	}()
}

// startIn makes the standby's own directory and starts it there.
func (pool *standbyPool) startIn(makeDir func() (string, error), spawn func(workDir string) (standbySpawn, error)) (*standbyProcess, error) {
	workDir, err := makeDir()
	if err != nil {
		return nil, err
	}
	launch, err := spawn(workDir)
	if err == nil {
		err = writeStandbyFiles(launch.files)
	}
	var process *standbyProcess
	if err == nil {
		process, err = pool.start(launch)
	}
	if err != nil {
		_ = os.RemoveAll(workDir)
		return nil, err
	}
	process.workDir = workDir
	process.token = launch.token
	return process, nil
}

// writeStandbyFiles writes the files of a launch. A file is written beside its path and renamed
// over it, so the named pipe of a pipe standby is replaced, never written into: an interpreter
// that opens the path later (for a traceback's source line) reads the code from the file.
func writeStandbyFiles(files []standbyFile) error {
	for _, file := range files {
		if file.dir {
			if err := os.MkdirAll(file.path, 0o700); err != nil {
				return err
			}
			continue
		}
		temporary, err := os.CreateTemp(filepath.Dir(file.path), "."+filepath.Base(file.path)+".*")
		if err != nil {
			return err
		}
		_, err = temporary.Write(file.data)
		if closeErr := temporary.Close(); err == nil {
			err = closeErr
		}
		if err == nil {
			err = os.Rename(temporary.Name(), file.path)
		}
		if err != nil {
			_ = os.Remove(temporary.Name())
			return err
		}
	}
	return nil
}

// remove takes process out of the waiting list; it reports false when a call took it first.
func (pool *standbyPool) remove(process *standbyProcess) bool {
	waiting := pool.idle[process.key]
	for index, candidate := range waiting {
		if candidate == process {
			pool.idle[process.key] = append(waiting[:index:index], waiting[index+1:]...)
			if len(pool.idle[process.key]) == 0 {
				delete(pool.idle, process.key)
			}
			pool.count--
			return true
		}
	}
	return false
}

func (pool *standbyPool) expire(process *standbyProcess) {
	pool.mu.Lock()
	removed := pool.remove(process)
	pool.mu.Unlock()
	if removed {
		process.discard()
	}
}

// died drops a standby that ended while it waited (its shim or template failed) and holds its
// key back, so a broken runner is not started again for every call.
func (pool *standbyPool) died(process *standbyProcess, idle time.Duration) {
	pool.mu.Lock()
	removed := pool.remove(process)
	if removed {
		pool.brokenUntil[process.key] = pool.now().Add(idle)
	}
	pool.mu.Unlock()
	if removed {
		process.timer.Stop()
		process.discard()
	}
}

// stats is how many standbys wait or start, how many of them are ready, and how many calls
// took one.
func (pool *standbyPool) stats() (count, ready, taken int) {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	for _, waiting := range pool.idle {
		ready += len(waiting)
	}
	return pool.count, ready, pool.taken
}

// stopAll stops every waiting standby.
func (pool *standbyPool) stopAll() {
	pool.mu.Lock()
	var stopped []*standbyProcess
	for _, waiting := range pool.idle {
		stopped = append(stopped, waiting...)
	}
	for _, process := range stopped {
		pool.remove(process)
		process.timer.Stop()
	}
	pool.mu.Unlock()
	for _, process := range stopped {
		process.discard()
	}
}

// startStandby starts the shim with the call's pipes in place: stdin, stdout and stderr, the
// code on fd 3 and the answer on fd 4. Nothing reads its output until a call takes it.
func startStandby(spawn standbySpawn) (*standbyProcess, error) {
	var opened []*os.File
	fail := func(err error) (*standbyProcess, error) {
		for _, file := range opened {
			_ = file.Close()
		}
		return nil, err
	}
	pipe := func() (*os.File, *os.File, error) {
		reader, writer, err := newBashPipe()
		if err == nil {
			opened = append(opened, reader, writer)
		}
		return reader, writer, err
	}
	stdoutReader, stdoutWriter, err := pipe()
	if err != nil {
		return fail(err)
	}
	stderrReader, stderrWriter, err := pipe()
	if err != nil {
		return fail(err)
	}
	stdinReader, stdinWriter, err := pipe()
	if err != nil {
		return fail(err)
	}
	var codeReader, codeWriter, controlReader, controlWriter *os.File
	if spawn.pipe != "" {
		// The interpreter opens its code file by path. Held open for reading and writing, the
		// named pipe never blocks that open, and its read waits for the call's code.
		if err := syscall.Mkfifo(spawn.pipe, 0o600); err != nil {
			return fail(err)
		}
		codeWriter, err = os.OpenFile(spawn.pipe, os.O_RDWR, 0)
		if err != nil {
			return fail(err)
		}
		opened = append(opened, codeWriter)
	} else {
		if codeReader, codeWriter, err = pipe(); err != nil {
			return fail(err)
		}
		if !spawn.gate {
			if controlReader, controlWriter, err = pipe(); err != nil {
				return fail(err)
			}
		}
	}
	resolved, err := resolveBashExecutable(spawn.file, spawn.environment, spawn.cwd)
	if err != nil {
		return fail(err)
	}
	launch, err := sandboxed(spawn.sandbox, sandboxPlace{workspace: spawn.workspace, cwd: spawn.cwd, tmp: spawn.tmp, sessionDir: spawn.sessionDir}, spawn.file, resolved, spawn.argv)
	if err != nil {
		return fail(err)
	}
	command := newBashCommand(launch.configured, launch.resolved, launch.args...)
	command.Dir = launch.dir
	command.Env = spawn.environment
	command.Stdin = stdinReader
	command.Stdout = stdoutWriter
	command.Stderr = stderrWriter
	childEnds := []*os.File{stdoutWriter, stderrWriter, stdinReader}
	switch {
	case spawn.gate:
		command.ExtraFiles = []*os.File{codeReader}
		childEnds = append(childEnds, codeReader)
	case spawn.pipe == "":
		// ExtraFiles[i] is fd 3+i in the child.
		command.ExtraFiles = []*os.File{codeReader, controlWriter}
		childEnds = append(childEnds, codeReader, controlWriter)
	}
	configureProcessGroup(command)
	if err := command.Start(); err != nil {
		return fail(err)
	}
	for _, childEnd := range childEnds {
		_ = childEnd.Close()
	}
	child := &startedBashChild{command: command, stdout: stdoutReader, stderr: stderrReader}
	child.watch()
	return &standbyProcess{child: child, stdin: stdinWriter, code: codeWriter, control: controlReader}, nil
}

// runStandbyCommand runs the call in a standby: it hands the code to the shim and, once the
// shim accepted it, supervises the process like a fresh one, with the call's input on stdin.
// A shim that declines, or ends without answering, ran nothing, so the call starts a fresh
// interpreter as it would have without standby.
func runStandbyCommand(ctx context.Context, request bashChildRequest, process *standbyProcess, code string) bashChildResult {
	started := time.Now()
	if ctx.Err() != nil {
		process.stop()
		return runBashCommand(ctx, request)
	}
	go func() {
		_, _ = io.WriteString(process.code, code)
		_ = process.code.Close()
	}()
	// A write the interpreter never reads (it opened the replaced file instead) ends with the call.
	defer process.code.Close()
	answer := make(chan bool, 1)
	if process.control == nil {
		// A pipe standby is the runner's own argv reading its own code file, and a gate
		// standby runs what a fresh start runs: nothing to decline.
		answer <- true
	} else {
		go func() {
			var reply [1]byte
			read, _ := process.control.Read(reply[:])
			answer <- read == 1 && reply[0] == standbyAccept
		}()
	}
	var timeout <-chan time.Time
	if request.timeout > 0 {
		timer := time.NewTimer(request.timeout)
		defer timer.Stop()
		timeout = timer.C
	}
	var accepted, expired bool
	select {
	case accepted = <-answer:
	case <-ctx.Done():
		// Stopped before the answer: end the standby, then see whether the code had started.
		// The supervision below reports the stop.
		signalProcessGroup(process.child.command, true)
		accepted = <-answer
	case <-timeout:
		signalProcessGroup(process.child.command, true)
		accepted, expired = <-answer, true
	}
	if process.control != nil {
		_ = process.control.Close()
	}
	if !accepted {
		process.stop()
		return runBashCommand(ctx, request)
	}
	if request.stdin == "" {
		_ = process.stdin.Close()
	} else {
		go func(writer *os.File, text string) {
			_, _ = io.WriteString(writer, text)
			_ = writer.Close()
		}(process.stdin, request.stdin)
	}
	// As with a fresh child, the input pipe closes when the call ends, so a writer blocked on a
	// descendant that never reads it is freed.
	defer process.stdin.Close()
	if request.collector == nil {
		request.collector = newBashOutputCollector(bashStreamLimits{}, bashStreamLimits{}, 0, true)
	}
	if request.killDeadline <= 0 {
		request.killDeadline = bashKillDeadline
	}
	if request.drainAfterExit <= 0 {
		request.drainAfterExit = bashDrainAfterExit
	}
	result := bashChildResult{spill: make(map[bashStream]bashSpillOutcome)}
	if expired {
		// The call's time ran out while its code started; the process is already killed.
		result.timedOut = true
		result.termination = "sigkill"
	}
	return superviseBashChild(ctx, request, process.child, started, result)
}
