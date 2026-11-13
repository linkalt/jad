package evalrun

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"jad/backend-go/internal/headless"
)

// Outcomes of one attempt of a case.
const (
	// OutcomePass: the run completed, every check exited 0 and every expect_text matched.
	OutcomePass = "pass"
	// OutcomeFail: the agent ran, but the run did not complete or a check or pattern failed.
	OutcomeFail = "fail"
	// OutcomeError: the harness could not get the agent to run (setup, session, refusal).
	OutcomeError = "error"
)

// checkWaitDelay is how long a check's output pipes may stay open after its time limit
// killed it, so a grandchild that keeps them open cannot hang the runner.
const checkWaitDelay = 5 * time.Second

// runIDRandomBytes is the size of the random part of a run id.
const runIDRandomBytes = 3

// Runner runs one headless turn; *headless.Runner implements it.
type Runner interface {
	Run(context.Context, headless.Request) (headless.Result, error)
}

// Root is a session root the case sessions can be written to.
type Root struct {
	ID        string
	Directory string
}

// Options are the parts of an eval run that do not come from the suite file.
type Options struct {
	Runner Runner
	// Roots are the served workspaces, in order.
	Roots []Root
	// Cases, when set, keeps only the cases whose name it matches.
	Cases *regexp.Regexp
	// Repeat and Parallel, when positive, replace the suite's values.
	Repeat   int
	Parallel int
	// Progress receives one line per finished attempt; nil discards them.
	Progress io.Writer
	// Now is the clock; nil uses time.Now.
	Now func() time.Time
	// RunID names this eval run in the session ids; empty uses the start time (UTC, to the
	// second) and a random suffix, so two runs started in the same second do not collide.
	RunID string
}

// Report is the result of an eval run.
type Report struct {
	Suite      string    `json:"suite"`
	RunID      string    `json:"run_id"`
	StartedAt  string    `json:"started_at"`
	DurationMS int64     `json:"duration_ms"`
	Summary    Summary   `json:"summary"`
	Results    []Attempt `json:"results"`
	// Interrupted is true when the run was stopped before every attempt ran.
	Interrupted bool `json:"interrupted,omitempty"`
}

// Summary counts the attempts and adds up their tokens.
type Summary struct {
	Total        int     `json:"total"`
	Passed       int     `json:"passed"`
	Failed       int     `json:"failed"`
	Errors       int     `json:"errors"`
	PassRate     float64 `json:"pass_rate"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	CachedTokens int64   `json:"cached_input_tokens"`
	// CostUSD adds up the cost of the attempts whose runs were priced (model file pricing);
	// absent when none was.
	CostUSD *float64 `json:"cost_usd,omitempty"`
	// StopHooksSkipped counts the attempts whose profile stop hooks were not run because the
	// run reached a cap. It does not change an outcome: the case's checks decide that.
	StopHooksSkipped int `json:"stop_hooks_skipped,omitempty"`
}

// Attempt is one run of one case.
type Attempt struct {
	Case    string `json:"case"`
	Attempt int    `json:"attempt"`
	Outcome string `json:"outcome"`
	// Reason says why an attempt did not pass.
	Reason     string `json:"reason,omitempty"`
	Session    string `json:"session,omitempty"`
	Workspace  string `json:"workspace,omitempty"`
	Kept       bool   `json:"workspace_kept"`
	DurationMS int64  `json:"duration_ms"`
	// Run is the headless result; absent when the agent never ran.
	Run    *headless.Result `json:"run,omitempty"`
	Setup  []Command        `json:"setup,omitempty"`
	Checks []Command        `json:"checks,omitempty"`
	Expect []Expectation    `json:"expect_text,omitempty"`
}

// Command is a setup or check command and how it ended.
type Command struct {
	Command    string `json:"command"`
	ExitCode   int    `json:"exit_code"`
	TimedOut   bool   `json:"timed_out,omitempty"`
	Output     string `json:"output,omitempty"`
	Truncated  bool   `json:"output_truncated,omitempty"`
	DurationMS int64  `json:"duration_ms"`
}

// Expectation is one expect_text pattern and whether the answer matched it.
type Expectation struct {
	Pattern string `json:"pattern"`
	Matched bool   `json:"matched"`
}

type job struct {
	index   int
	c       *Case
	attempt int
}

// Run runs every selected case of suite, Repeat times each, at most Parallel at once.
func Run(ctx context.Context, suite *Suite, options Options) (Report, error) {
	if options.Runner == nil {
		return Report{}, errors.New("no runner")
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	root, err := pickRoot(options.Roots, suite.SessionRoot)
	if err != nil {
		return Report{}, err
	}
	repeat, parallel := suite.Repeat, suite.Parallel
	if options.Repeat > 0 {
		repeat = options.Repeat
	}
	if options.Parallel > 0 {
		parallel = options.Parallel
	}
	var jobs []job
	for index := range suite.Cases {
		one := &suite.Cases[index]
		if options.Cases != nil && !options.Cases.MatchString(one.Name) {
			continue
		}
		for attempt := 1; attempt <= repeat; attempt++ {
			jobs = append(jobs, job{index: len(jobs), c: one, attempt: attempt})
		}
	}
	if len(jobs) == 0 {
		return Report{}, errors.New("no case matches")
	}
	started := now()
	stamp := options.RunID
	if stamp == "" {
		suffix := make([]byte, runIDRandomBytes)
		if _, err := rand.Read(suffix); err != nil {
			return Report{}, err
		}
		stamp = started.UTC().Format("20060102T150405") + "-" + hex.EncodeToString(suffix)
	}
	results := make([]Attempt, len(jobs))
	ran := make([]bool, len(jobs))
	queue := make(chan job)
	var wait sync.WaitGroup
	var progress sync.Mutex
	for range min(parallel, len(jobs)) {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for next := range queue {
				result := runAttempt(ctx, suite, options.Runner, root, stamp, next.c, next.attempt)
				results[next.index], ran[next.index] = result, true
				if options.Progress != nil {
					progress.Lock()
					writeProgress(options.Progress, result)
					progress.Unlock()
				}
			}
		}()
	}
	interrupted := false
send:
	for _, next := range jobs {
		select {
		case queue <- next:
		case <-ctx.Done():
			interrupted = true
			break send
		}
	}
	close(queue)
	wait.Wait()

	report := Report{Suite: suite.Name, RunID: stamp, StartedAt: started.UTC().Format(time.RFC3339), Interrupted: interrupted || ctx.Err() != nil}
	for index, result := range results {
		if ran[index] {
			report.Results = append(report.Results, result)
		}
	}
	report.Summary = summarize(report.Results)
	report.DurationMS = now().Sub(started).Milliseconds()
	return report, nil
}

func pickRoot(roots []Root, id string) (Root, error) {
	if len(roots) == 0 {
		return Root{}, errors.New("no workspace is served")
	}
	if id == "" {
		return roots[0], nil
	}
	for _, root := range roots {
		if root.ID == id {
			return root, nil
		}
	}
	return Root{}, fmt.Errorf("session_root %q is not a served workspace", id)
}

// runAttempt runs one attempt; it never returns an error, only an attempt that says what
// went wrong.
//
// result is a named result: the deferred steps (workspace retention, duration) change the
// value the caller receives.
func runAttempt(ctx context.Context, suite *Suite, runner Runner, root Root, stamp string, c *Case, attempt int) (result Attempt) {
	began := time.Now()
	result = Attempt{Case: c.Name, Attempt: attempt}
	defer func() { result.DurationMS = time.Since(began).Milliseconds() }()

	attemptDir, err := os.MkdirTemp(suite.WorkDir, "jad-eval-"+c.Name+"-")
	if err != nil {
		result.Outcome, result.Reason = OutcomeError, fmt.Sprintf("create the workspace: %v", err)
		return result
	}
	workspace := filepath.Join(attemptDir, "workspace")
	answerFile := filepath.Join(attemptDir, "answer.txt")
	result.Workspace = workspace
	defer func() {
		keep := suite.KeepWorkspaces == KeepAlways || (suite.KeepWorkspaces == KeepFailed && result.Outcome != OutcomePass)
		result.Kept = keep
		if !keep {
			_ = os.RemoveAll(attemptDir)
		}
	}()
	if err := prepareWorkspace(c.Workspace, workspace); err != nil {
		result.Outcome, result.Reason = OutcomeError, fmt.Sprintf("copy the workspace: %v", err)
		return result
	}
	environment := append(os.Environ(),
		"JAD_EVAL_SUITE="+suite.Name, "JAD_EVAL_CASE="+c.Name, fmt.Sprintf("JAD_EVAL_ATTEMPT=%d", attempt),
		"JAD_EVAL_WORKSPACE="+workspace, "JAD_EVAL_ANSWER_FILE="+answerFile)
	for _, line := range c.Setup {
		command := runShell(ctx, suite, workspace, environment, line)
		result.Setup = append(result.Setup, command)
		if command.ExitCode != 0 {
			result.Outcome, result.Reason = OutcomeError, fmt.Sprintf("setup command failed: %s", line)
			return result
		}
	}

	sessionID := sessionName(suite.Name, stamp, c.Name, attempt)
	if err := writeSession(root, sessionID, suite, workspace, c, attempt); err != nil {
		result.Outcome, result.Reason = OutcomeError, fmt.Sprintf("write the session: %v", err)
		return result
	}
	result.Session = root.ID + ":" + sessionID
	run, err := runner.Run(ctx, headless.Request{
		Session:  result.Session,
		Profile:  suite.profileOf(c),
		Model:    suite.modelOf(c),
		Input:    c.Prompt,
		Timeout:  time.Duration(suite.timeoutOf(c)) * time.Millisecond,
		Commands: suite.commandsOf(c),
		Plan:     suite.planOf(c),
		Output:   c.output,
	})
	if err != nil {
		result.Outcome, result.Reason = OutcomeError, err.Error()
		return result
	}
	result.Run = &run
	_ = os.WriteFile(answerFile, []byte(run.Text), 0o600)

	reasons := []string{}
	switch run.Status {
	case headless.StatusCompleted:
	case headless.StatusInvalidOutput:
		reasons = append(reasons, run.Error)
	default:
		reasons = append(reasons, "run "+run.Status)
	}
	for _, line := range c.Checks {
		command := runShell(ctx, suite, workspace, environment, line)
		result.Checks = append(result.Checks, command)
		if command.ExitCode != 0 {
			reasons = append(reasons, fmt.Sprintf("check exited %d: %s", command.ExitCode, line))
		}
	}
	for _, pattern := range c.expect {
		matched := pattern.MatchString(run.Text)
		result.Expect = append(result.Expect, Expectation{Pattern: pattern.String(), Matched: matched})
		if !matched {
			reasons = append(reasons, fmt.Sprintf("answer does not match %q", pattern.String()))
		}
	}
	if len(reasons) == 0 {
		result.Outcome = OutcomePass
	} else {
		result.Outcome, result.Reason = OutcomeFail, strings.Join(reasons, "; ")
	}
	return result
}

// sessionName builds a session id that sorts by suite and time and fits the session id
// pattern (at most 128 characters).
func sessionName(suite, stamp, caseName string, attempt int) string {
	const limit = 128
	tail := fmt.Sprintf("-%s-%s-%d", stamp, caseName, attempt)
	head := "eval-" + suite
	if len(head)+len(tail) > limit {
		head = head[:limit-len(tail)]
	}
	return head + tail
}

// writeSession writes <root>/<id>.json, refusing to replace an existing file.
func writeSession(root Root, id string, suite *Suite, workspace string, c *Case, attempt int) error {
	session := map[string]any{}
	for key, value := range suite.Session {
		session[key] = value
	}
	session["schema_version"] = 1
	session["cwd"] = workspace
	session["description"] = fmt.Sprintf("Eval %s, case %s, attempt %d.", suite.Name, c.Name, attempt)
	encoded, err := json.MarshalIndent(session, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(root.Directory, 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(filepath.Join(root.Directory, id+".json"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(append(encoded, '\n')); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

// prepareWorkspace creates target and copies source into it, when source is set. Regular
// files keep their mode bits and symbolic links are recreated; other kinds are skipped.
func prepareWorkspace(source, target string) error {
	if err := os.Mkdir(target, 0o755); err != nil {
		return err
	}
	if source == "" {
		return nil
	}
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(source, path)
		if err != nil || rel == "." {
			return err
		}
		destination := filepath.Join(target, rel)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case entry.IsDir():
			return os.Mkdir(destination, info.Mode().Perm()|0o700)
		case info.Mode()&fs.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, destination)
		case info.Mode().IsRegular():
			return copyFile(path, destination, info.Mode().Perm())
		}
		return nil
	})
}

func copyFile(source, destination string, mode fs.FileMode) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// runShell runs one command line in dir with the suite's shell and time limit. Its
// combined output is kept up to check_output_max_bytes, the end of it when longer.
func runShell(ctx context.Context, suite *Suite, dir string, environment []string, line string) Command {
	began := time.Now()
	limited, cancel := context.WithTimeout(ctx, time.Duration(suite.CheckTimeoutMS)*time.Millisecond)
	defer cancel()
	args := append(append([]string(nil), suite.Shell[1:]...), line)
	command := exec.CommandContext(limited, suite.Shell[0], args...)
	command.Dir = dir
	command.Env = environment
	output := &tailBuffer{limit: suite.CheckOutputMaxBytes}
	command.Stdout, command.Stderr = output, output
	command.WaitDelay = checkWaitDelay
	startInGroup(command)
	err := command.Run()
	result := Command{Command: line, Output: output.String(), Truncated: output.dropped, DurationMS: time.Since(began).Milliseconds()}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case limited.Err() != nil:
		result.ExitCode, result.TimedOut = -1, errors.Is(limited.Err(), context.DeadlineExceeded)
	case errors.As(err, &exitErr):
		result.ExitCode = exitErr.ExitCode()
		if result.ExitCode == 0 {
			result.ExitCode = -1
		}
	default:
		result.ExitCode = -1
		result.Output = strings.TrimSpace(result.Output + "\n" + err.Error())
	}
	return result
}

// tailBuffer keeps the last limit bytes written to it.
type tailBuffer struct {
	mu      sync.Mutex
	limit   int
	data    []byte
	dropped bool
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.data = append(b.data, p...)
	if over := len(b.data) - b.limit; over > 0 {
		b.data = append(b.data[:0], b.data[over:]...)
		b.dropped = true
	}
	return len(p), nil
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.data)
}

func summarize(results []Attempt) Summary {
	summary := Summary{Total: len(results)}
	for _, result := range results {
		switch result.Outcome {
		case OutcomePass:
			summary.Passed++
		case OutcomeFail:
			summary.Failed++
		default:
			summary.Errors++
		}
		if result.Run != nil && result.Run.StopHooksSkipped != "" {
			summary.StopHooksSkipped++
		}
		if result.Run != nil && result.Run.CostUSD != nil {
			total := *result.Run.CostUSD
			if summary.CostUSD != nil {
				total += *summary.CostUSD
			}
			summary.CostUSD = &total
		}
		if result.Run == nil || len(result.Run.Usage) == 0 {
			continue
		}
		var usage struct {
			InputTokens       int64 `json:"input_tokens"`
			OutputTokens      int64 `json:"output_tokens"`
			CachedInputTokens int64 `json:"cached_input_tokens"`
		}
		if json.Unmarshal(result.Run.Usage, &usage) == nil {
			summary.InputTokens += usage.InputTokens
			summary.OutputTokens += usage.OutputTokens
			summary.CachedTokens += usage.CachedInputTokens
		}
	}
	if summary.Total > 0 {
		summary.PassRate = float64(summary.Passed) / float64(summary.Total)
	}
	return summary
}

func writeProgress(w io.Writer, result Attempt) {
	label := strings.ToUpper(result.Outcome)
	line := fmt.Sprintf("%-5s %s#%d (%.1fs)", label, result.Case, result.Attempt, float64(result.DurationMS)/1000)
	if result.Reason != "" {
		line += " " + result.Reason
	}
	if result.Run != nil && result.Run.StopHooksSkipped != "" {
		line += " " + result.Run.StopHooksSkipped
	}
	_, _ = fmt.Fprintln(w, line)
}
