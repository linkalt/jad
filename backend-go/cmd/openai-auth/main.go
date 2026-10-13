// Command openai-auth signs in to a ChatGPT account, refreshes its tokens and reports the state
// of the credential file that the Go backend reads for model.json auth_mode "openai_oauth".
// The backend never writes that file; this command is its only writer.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"jad/backend-go/internal/openaiauth"
)

// Exit statuses. status uses 0, 1 and 2 as documented in the README; the other commands use
// 0, 1 and exitNeedLogin.
const (
	exitOK        = 0
	exitFailed    = 1
	exitUsage     = 64
	exitNeedLogin = 3
)

const usage = `Usage: openai-auth <command> [flags]

Commands:
  login     Sign in with a browser and store the tokens
  refresh   Trade the stored refresh token for new tokens
  status    Report the state of the credential file (never prints a token)

Common flag:
  --config <path>   openai-auth config file (defaults are used when omitted)

Run "openai-auth <command> -h" for the flags of one command.
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr, time.Now)
	stop()
	os.Exit(code)
}

// run executes one command line and returns the exit status.
func run(ctx context.Context, args []string, stdout, stderr io.Writer, now func() time.Time) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return exitUsage
	}
	command, rest := args[0], args[1:]
	switch command {
	case "login":
		return runLogin(ctx, rest, stdout, stderr, now)
	case "refresh":
		return runRefresh(ctx, rest, stdout, stderr, now)
	case "status":
		return runStatus(rest, stdout, stderr, now)
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usage)
		return exitOK
	}
	fmt.Fprintf(stderr, "openai-auth: unknown command %q\n\n%s", command, usage)
	return exitUsage
}

// setup holds what every command needs after its flags are parsed.
type setup struct {
	config *openaiauth.Config
	store  *openaiauth.Store
	client *openaiauth.Client
}

// parseFlags parses args into set and loads the config named by --config. ok is false when the
// command should stop with the returned exit status.
func parseFlags(name string, args []string, stderr io.Writer, define func(*flag.FlagSet)) (set *flag.FlagSet, configPath *string, status int, ok bool) {
	set = flag.NewFlagSet("openai-auth "+name, flag.ContinueOnError)
	set.SetOutput(stderr)
	configPath = set.String("config", "", "path to the openai-auth JSON config (defaults are used when empty)")
	if define != nil {
		define(set)
	}
	if err := set.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return set, configPath, exitOK, false
		}
		return set, configPath, exitUsage, false
	}
	if set.NArg() > 0 {
		fmt.Fprintf(stderr, "openai-auth %s: unexpected argument %q\n", name, set.Arg(0))
		return set, configPath, exitUsage, false
	}
	return set, configPath, exitOK, true
}

func load(configPath string, stderr io.Writer) (*setup, bool) {
	config, err := openaiauth.LoadConfig(configPath)
	if err != nil {
		fmt.Fprintf(stderr, "openai-auth: %v\n", err)
		return nil, false
	}
	path, err := config.AuthFilePath()
	if err != nil {
		fmt.Fprintf(stderr, "openai-auth: config: auth_file: %v\n", err)
		return nil, false
	}
	return &setup{
		config: config,
		store:  &openaiauth.Store{Path: path, KeepBackup: config.KeepBackup},
		client: &openaiauth.Client{Config: config},
	}, true
}

func runLogin(ctx context.Context, args []string, stdout, stderr io.Writer, now func() time.Time) int {
	var force, noBrowser *bool
	_, configPath, status, ok := parseFlags("login", args, stderr, func(set *flag.FlagSet) {
		force = set.Bool("force", false, "replace an existing credential file")
		noBrowser = set.Bool("no-browser", false, "only print the URL; do not open a browser")
	})
	if !ok {
		return status
	}
	s, ok := load(*configPath, stderr)
	if !ok {
		return exitFailed
	}
	options := openaiauth.LoginOptions{Force: *force, Out: stderr, Now: now}
	if s.config.OpenBrowser && !*noBrowser {
		options.OpenBrowser = openBrowser
	}
	if err := openaiauth.Login(ctx, s.store, s.client, options); err != nil {
		fmt.Fprintf(stderr, "openai-auth login: %v\n", err)
		return exitFailed
	}
	fmt.Fprintf(stdout, "Signed in. Credentials stored in %s\n", s.store.Path)
	return exitOK
}

func runRefresh(ctx context.Context, args []string, stdout, stderr io.Writer, now func() time.Time) int {
	var ifOlderThan *time.Duration
	_, configPath, status, ok := parseFlags("refresh", args, stderr, func(set *flag.FlagSet) {
		ifOlderThan = set.Duration("if-older-than", 0, "skip the refresh when the tokens were refreshed more recently than this (for example 55m); 0 always refreshes")
	})
	if !ok {
		return status
	}
	if *ifOlderThan < 0 {
		fmt.Fprintln(stderr, "openai-auth refresh: --if-older-than must not be negative")
		return exitUsage
	}
	s, ok := load(*configPath, stderr)
	if !ok {
		return exitFailed
	}
	result, err := openaiauth.Refresh(ctx, s.store, s.client, openaiauth.RefreshOptions{IfOlderThan: *ifOlderThan, Now: now})
	if err != nil {
		fmt.Fprintf(stderr, "openai-auth refresh: %v\n", err)
		if errors.Is(err, openaiauth.ErrNeedLogin) {
			return exitNeedLogin
		}
		return exitFailed
	}
	if !result.Refreshed {
		fmt.Fprintf(stdout, "Not refreshed: the tokens are %s old, less than %s.\n", result.Age.Round(time.Second), *ifOlderThan)
		return exitOK
	}
	fmt.Fprintf(stdout, "Refreshed. Credentials stored in %s\n", s.store.Path)
	return exitOK
}
