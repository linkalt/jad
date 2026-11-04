package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"

	"jad/backend-go/internal/evalrun"
)

const evalUsage = `usage: jad eval [--out <report.json>] [--cases <regexp>] [--repeat <n>] [--parallel <n>]
                [--no-commands] [--log-level <level>] <suite.json>

Runs an eval suite (sample/schemes/eval-suite.schema.json): each case gets a fresh copy of
its workspace and a new session in the "eval" category, one headless turn, then its checks.
Progress goes to stderr; the JSON report goes to --out, or to stdout without it.

A case prompt that starts with a slash command runs it as jad run does: /prompt:,
/template:, /skill:, /mcp:<server>:<prompt> and a custom command send their text, and
/plan runs the case in plan mode (so does "plan": true on the suite or the case). A case
prompt may not be /compact, /reset or /state, which run no turn. "commands": false on the
suite or a case, or --no-commands for the whole run, sends prompts as written.

Exit status: 0 every attempt passed, 1 an attempt failed or errored, 2 usage error,
130 interrupted.
`

func runEvalCommand(args []string, streams commandStreams) error {
	flags := flag.NewFlagSet("jad eval", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	out := flags.String("out", "", "write the JSON report to this file")
	cases := flags.String("cases", "", "run only the cases whose name matches this regular expression")
	repeat := flags.Int("repeat", 0, "attempts per case; replaces the suite's repeat")
	parallel := flags.Int("parallel", 0, "attempts at once; replaces the suite's parallel")
	logLevel := flags.String("log-level", "", "override server.log_level")
	noCommands := flags.Bool("no-commands", false, "send every case prompt as written")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, _ = io.WriteString(streams.stdout, evalUsage)
			return &exitError{code: 0}
		}
		return usageError("%v\n\n%s", err, evalUsage)
	}
	if flags.NArg() != 1 {
		return usageError("name one suite file\n\n%s", evalUsage)
	}
	if *repeat < 0 || *parallel < 0 {
		return usageError("--repeat and --parallel must not be negative")
	}
	var filter *regexp.Regexp
	if *cases != "" {
		compiled, err := regexp.Compile(*cases)
		if err != nil {
			return usageError("--cases: %v", err)
		}
		filter = compiled
	}
	suite, err := evalrun.LoadSuiteWith(flags.Arg(0), evalrun.LoadOptions{NoCommands: *noCommands})
	if err != nil {
		return &exitError{code: exitFailed, err: err}
	}
	config, runner, closeService, err := headlessService(*logLevel, streams.stderr)
	if err != nil {
		return err
	}
	defer closeService()
	roots := make([]evalrun.Root, 0, len(config.Roots))
	for _, root := range config.Roots {
		roots = append(roots, evalrun.Root{ID: root.ID, Directory: root.Directory})
	}
	ctx, stop := interruptContext()
	defer stop()
	report, err := evalrun.Run(ctx, suite, evalrun.Options{
		Runner: runner, Roots: roots, Cases: filter, Repeat: *repeat, Parallel: *parallel, Progress: streams.stderr,
	})
	if err != nil {
		return &exitError{code: exitFailed, err: err}
	}
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return &exitError{code: exitFailed, err: err}
	}
	encoded = append(encoded, '\n')
	if *out != "" {
		if err := os.WriteFile(*out, encoded, 0o644); err != nil {
			return &exitError{code: exitFailed, err: fmt.Errorf("write the report: %w", err)}
		}
	} else if _, err := streams.stdout.Write(encoded); err != nil {
		return &exitError{code: exitFailed, err: fmt.Errorf("write the report: %w", err)}
	}
	summary := report.Summary
	_, _ = fmt.Fprintf(streams.stderr, "%s: %d/%d passed (%.0f%%), %d failed, %d errors, %d input + %d output tokens\n",
		report.Suite, summary.Passed, summary.Total, summary.PassRate*100, summary.Failed, summary.Errors, summary.InputTokens, summary.OutputTokens)
	switch {
	case report.Interrupted:
		return &exitError{code: exitInterrupted}
	case summary.Passed != summary.Total:
		return &exitError{code: exitFailed}
	}
	return nil
}
