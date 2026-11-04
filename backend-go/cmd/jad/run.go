package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"jad/backend-go/internal/app"
	"jad/backend-go/internal/headless"
	"jad/backend-go/internal/logging"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools/native"
	"jad/backend-go/internal/tracing"
)

// Exit codes of "jad run" and "jad eval". They follow common shell practice: 2 for a usage
// error, 124 for a time limit (as timeout(1)), 130 for an interrupt (128 + SIGINT).
const (
	exitFailed      = 1
	exitUsage       = 2
	exitTimedOut    = 124
	exitInterrupted = 130
)

// Output formats of "jad run".
const (
	formatText  = "text"
	formatJSON  = "json"
	formatJSONL = "jsonl"
)

// exitError ends the process with code. Its error, when set, is printed on stderr first.
type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string {
	if e.err == nil {
		return fmt.Sprintf("exit status %d", e.code)
	}
	return e.err.Error()
}

func (e *exitError) Unwrap() error { return e.err }

func usageError(format string, args ...any) error {
	return &exitError{code: exitUsage, err: fmt.Errorf(format, args...)}
}

// commandStreams are the standard streams a command reads and writes.
type commandStreams struct {
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
}

const runUsage = `usage: jad run --session <ref> [--profile <category/name>] [--model <category/name>]
               [--format text|json|jsonl] [--timeout <duration>] [--log-level <level>] [prompt | -]

Runs one turn of a session without a server and exits. The prompt is the remaining
arguments, or standard input when there are none or the only one is "-".

Formats: text prints the final answer; json prints one result object; jsonl prints every
run-stream event as {"event","data"} and ends with {"event":"result","data":<result>}.

Exit status: 0 completed, 1 failed, 2 usage error, 124 timed out, 130 interrupted.
`

// runOptions are the parsed flags of "jad run".
type runOptions struct {
	request  headless.Request
	format   string
	logLevel string
}

func parseRunOptions(args []string, streams commandStreams) (runOptions, error) {
	flags := flag.NewFlagSet("jad run", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var options runOptions
	flags.StringVar(&options.request.Session, "session", "", "session reference")
	flags.StringVar(&options.request.Profile, "profile", "", "profile, <category>/<name>")
	flags.StringVar(&options.request.Model, "model", "", "model, <category>/<name>")
	flags.StringVar(&options.format, "format", formatText, "text, json or jsonl")
	flags.DurationVar(&options.request.Timeout, "timeout", 0, "stop the run after this long; 0 means no limit")
	flags.StringVar(&options.logLevel, "log-level", "", "override server.log_level")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, _ = io.WriteString(streams.stdout, runUsage)
			return runOptions{}, &exitError{code: 0}
		}
		return runOptions{}, usageError("%v\n\n%s", err, runUsage)
	}
	if strings.TrimSpace(options.request.Session) == "" {
		return runOptions{}, usageError("--session is required\n\n%s", runUsage)
	}
	switch options.format {
	case formatText, formatJSON, formatJSONL:
	default:
		return runOptions{}, usageError("--format must be text, json or jsonl, not %q", options.format)
	}
	if options.request.Timeout < 0 {
		return runOptions{}, usageError("--timeout must not be negative")
	}
	prompt, err := readPrompt(flags.Args(), streams.stdin)
	if err != nil {
		return runOptions{}, err
	}
	options.request.Input = prompt
	return options, nil
}

// readPrompt joins the prompt arguments, or reads standard input when there are none or
// the only one is "-".
func readPrompt(args []string, stdin io.Reader) (string, error) {
	if len(args) > 0 && !(len(args) == 1 && args[0] == "-") {
		return strings.Join(args, " "), nil
	}
	if stdin == nil {
		return "", usageError("no prompt given")
	}
	raw, err := io.ReadAll(stdin)
	if err != nil {
		return "", fmt.Errorf("read the prompt from standard input: %w", err)
	}
	if strings.TrimSpace(string(raw)) == "" {
		return "", usageError("the prompt is empty")
	}
	return string(raw), nil
}

// headlessService loads the configuration and starts a headless service. logLevel, when
// set, replaces server.log_level; the log goes to server.log_file or to stderr, never to
// stdout, which carries the command's output.
func headlessService(logLevel string, stderr io.Writer) (app.Config, *headless.Runner, func(), error) {
	config, err := app.LoadConfig(sessionfs.LoadOptions{})
	if err != nil {
		return app.Config{}, nil, nil, fmt.Errorf("load server configuration: %w", err)
	}
	if logLevel != "" {
		config.Log.Level = logLevel
	}
	logger, closeLog, err := logging.New(config.Log, stderr)
	if err != nil {
		return app.Config{}, nil, nil, fmt.Errorf("configure logging: %w", err)
	}
	config.HTTP.Logger = logger
	for _, warning := range config.RootSettingWarnings {
		logger.Warn(warning)
	}
	shutdownTracing, err := tracing.Setup(config.Tracing, logger)
	if err != nil {
		closeLog.Close()
		return app.Config{}, nil, nil, err
	}
	service, runner, err := app.NewHeadless(config)
	if err != nil {
		_ = tracing.Shutdown(shutdownTracing, config.Tracing)
		closeLog.Close()
		return app.Config{}, nil, nil, fmt.Errorf("initialize Go backend: %w", err)
	}
	// The service closes first, so every run span has ended before the spans are flushed.
	return config, runner, func() {
		service.Close()
		// Language servers the lsp tool started outlive catalogs; they end with the command.
		native.ShutdownLanguageServers()
		if err := tracing.Shutdown(shutdownTracing, config.Tracing); err != nil {
			logger.Warn("tracing shutdown failed", "error", err)
		}
		closeLog.Close()
	}, nil
}

// interruptContext is cancelled by SIGINT or SIGTERM.
func interruptContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func runRunCommand(args []string, streams commandStreams) error {
	options, err := parseRunOptions(args, streams)
	if err != nil {
		return err
	}
	_, runner, closeService, err := headlessService(options.logLevel, streams.stderr)
	if err != nil {
		return err
	}
	defer closeService()
	ctx, stop := interruptContext()
	defer stop()
	return runOnce(ctx, runner, options, streams)
}

// runOnce sends one turn and writes its output in the chosen format.
func runOnce(ctx context.Context, runner *headless.Runner, options runOptions, streams commandStreams) error {
	encoder := json.NewEncoder(streams.stdout)
	encoder.SetEscapeHTML(false)
	var writeErr error
	if options.format == formatJSONL {
		options.request.OnEvent = func(event headless.Event) {
			if writeErr == nil {
				writeErr = encoder.Encode(event)
			}
		}
	}
	result, err := runner.Run(ctx, options.request)
	if err != nil {
		return &exitError{code: exitFailed, err: err}
	}
	switch options.format {
	case formatText:
		if result.Text != "" {
			_, writeErr = io.WriteString(streams.stdout, strings.TrimRight(result.Text, "\n")+"\n")
		}
	case formatJSON:
		writeErr = encoder.Encode(result)
	case formatJSONL:
		if writeErr == nil {
			writeErr = encoder.Encode(struct {
				Name string          `json:"event"`
				Data headless.Result `json:"data"`
			}{Name: "result", Data: result})
		}
	}
	if writeErr != nil {
		return &exitError{code: exitFailed, err: fmt.Errorf("write the output: %w", writeErr)}
	}
	return resultExit(result, options.format == formatText, streams.stderr)
}

// resultExit maps a result to the command's exit. In text format the failure message is
// printed on stderr, since stdout carries only the answer.
func resultExit(result headless.Result, describe bool, stderr io.Writer) error {
	code := 0
	switch result.Status {
	case headless.StatusCompleted:
		return nil
	case headless.StatusTimedOut:
		code = exitTimedOut
	case headless.StatusAborted:
		code = exitInterrupted
	default:
		code = exitFailed
	}
	if describe && result.Error != "" {
		_, _ = fmt.Fprintf(stderr, "jad: run %s: %s\n", result.Status, result.Error)
	}
	return &exitError{code: code}
}
