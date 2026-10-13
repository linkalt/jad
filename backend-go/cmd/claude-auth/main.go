// Command claude-auth signs in to a Claude account (Pro, Max, Team or Enterprise), refreshes its
// tokens and reports the state of the credential file that the Anthropic adapter reads
// (upstream.credential_file). The adapter never writes that file; this command is its only writer.
//
// The sign-in endpoints are not documented by Anthropic, and its terms limit subscription
// credentials to its own applications. See internal/claudeauth and the README.
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

	"jad/backend-go/internal/claudeauth"
)

// Exit statuses. status uses 0, 1 and 2 as documented in the README; the other commands use
// 0, 1 and exitNeedLogin.
const (
	exitOK        = 0
	exitFailed    = 1
	exitUsage     = 64
	exitNeedLogin = 3
)

const usage = `Usage: claude-auth <command> [flags]

Commands:
  login     Sign in with a browser and store the tokens
  refresh   Trade the stored refresh token for new tokens
  status    Report the state of the credential file (never prints a token)

Common flag:
  --config <path>   claude-auth config file (defaults are used when omitted)

Run "claude-auth <command> -h" for the flags of one command.
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr, time.Now)
	stop()
	os.Exit(code)
}

// run executes one command line and returns the exit status.
func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, now func() time.Time) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return exitUsage
	}
	command, rest := args[0], args[1:]
	switch command {
	case "login":
		return runLogin(ctx, rest, stdin, stdout, stderr, now)
	case "refresh":
		return runRefresh(ctx, rest, stdout, stderr, now)
	case "status":
		return runStatus(rest, stdout, stderr, now)
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usage)
		return exitOK
	}
	fmt.Fprintf(stderr, "claude-auth: unknown command %q\n\n%s", command, usage)
	return exitUsage
}

// setup holds what every command needs after its flags are parsed.
type setup struct {
	config *claudeauth.Config
	store  *claudeauth.Store
	client *claudeauth.Client
}

// parseFlags parses args into set and loads the config named by --config. ok is false when the
// command should stop with the returned exit status.
func parseFlags(name string, args []string, stderr io.Writer, define func(*flag.FlagSet)) (configPath *string, status int, ok bool) {
	set := flag.NewFlagSet("claude-auth "+name, flag.ContinueOnError)
	set.SetOutput(stderr)
	configPath = set.String("config", "", "path to the claude-auth JSON config (defaults are used when empty)")
	if define != nil {
		define(set)
	}
	if err := set.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return configPath, exitOK, false
		}
		return configPath, exitUsage, false
	}
	if set.NArg() > 0 {
		fmt.Fprintf(stderr, "claude-auth %s: unexpected argument %q\n", name, set.Arg(0))
		return configPath, exitUsage, false
	}
	return configPath, exitOK, true
}

func load(configPath string, stderr io.Writer) (*setup, bool) {
	config, err := claudeauth.LoadConfig(configPath)
	if err != nil {
		fmt.Fprintf(stderr, "claude-auth: %v\n", err)
		return nil, false
	}
	path, err := config.AuthFilePath()
	if err != nil {
		fmt.Fprintf(stderr, "claude-auth: config: auth_file: %v\n", err)
		return nil, false
	}
	return &setup{
		config: config,
		store:  &claudeauth.Store{Path: path, KeepBackup: config.KeepBackup},
		client: &claudeauth.Client{Config: config},
	}, true
}

func runLogin(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, now func() time.Time) int {
	var force, noBrowser, copyCode *bool
	configPath, status, ok := parseFlags("login", args, stderr, func(set *flag.FlagSet) {
		force = set.Bool("force", false, "replace an existing credential file")
		noBrowser = set.Bool("no-browser", false, "only print the URL; do not open a browser")
		copyCode = set.Bool("copy-code", false, "do not listen for a redirect: paste the code the sign-in page shows (for a browser on another machine)")
	})
	if !ok {
		return status
	}
	s, ok := load(*configPath, stderr)
	if !ok {
		return exitFailed
	}
	options := claudeauth.LoginOptions{Force: *force, CopyCode: *copyCode, In: stdin, Out: stderr, Now: now}
	if s.config.OpenBrowser && !*noBrowser {
		options.OpenBrowser = openBrowser
	}
	if err := claudeauth.Login(ctx, s.store, s.client, options); err != nil {
		fmt.Fprintf(stderr, "claude-auth login: %v\n", err)
		return exitFailed
	}
	fmt.Fprintf(stdout, "Signed in. Credentials stored in %s\n", s.store.Path)
	return exitOK
}

func runRefresh(ctx context.Context, args []string, stdout, stderr io.Writer, now func() time.Time) int {
	var ifOlderThan, ifExpiresWithin *time.Duration
	configPath, status, ok := parseFlags("refresh", args, stderr, func(set *flag.FlagSet) {
		ifOlderThan = set.Duration("if-older-than", 0, "skip the refresh when the tokens were refreshed more recently than this (for example 55m); 0 does not apply the rule")
		ifExpiresWithin = set.Duration("if-expires-within", 0, "skip the refresh unless the access token ends within this time (for example 30m); 0 does not apply the rule")
	})
	if !ok {
		return status
	}
	if *ifOlderThan < 0 || *ifExpiresWithin < 0 {
		fmt.Fprintln(stderr, "claude-auth refresh: --if-older-than and --if-expires-within must not be negative")
		return exitUsage
	}
	s, ok := load(*configPath, stderr)
	if !ok {
		return exitFailed
	}
	result, err := claudeauth.Refresh(ctx, s.store, s.client, claudeauth.RefreshOptions{
		IfOlderThan: *ifOlderThan, IfExpiresWithin: *ifExpiresWithin, Now: now,
	})
	if err != nil {
		fmt.Fprintf(stderr, "claude-auth refresh: %v\n", err)
		if errors.Is(err, claudeauth.ErrNeedLogin) {
			return exitNeedLogin
		}
		return exitFailed
	}
	if !result.Refreshed {
		fmt.Fprintf(stdout, "Not refreshed: %s\n", skipReason(result, *ifOlderThan, *ifExpiresWithin))
		return exitOK
	}
	fmt.Fprintf(stdout, "Refreshed. Credentials stored in %s\n", s.store.Path)
	return exitOK
}

// skipReason names the rule that stopped the refresh.
func skipReason(result *claudeauth.RefreshResult, olderThan, expiresWithin time.Duration) string {
	if olderThan > 0 && result.AgeKnown && result.Age < olderThan {
		return fmt.Sprintf("the tokens are %s old, less than %s.", result.Age.Round(time.Second), olderThan)
	}
	return fmt.Sprintf("the access token has %s left, more than %s.", result.Remaining.Round(time.Second), expiresWithin)
}
