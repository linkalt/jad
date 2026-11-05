package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"jad/backend-go/internal/catalog"
	"jad/backend-go/internal/sessionfs"
	mcpclient "jad/backend-go/internal/tools/mcp"
)

const mcpUsage = `usage: jad mcp login  [--root <id>] [--timeout <duration>] <category>/<name>
       jad mcp logout [--root <id>] <category>/<name>

login signs in to the MCP server of mcp/<category>/<name>.json, whose transport.oauth uses
grant authorization_code. It prints the address to open in a browser and waits for the
browser to come back to transport.oauth.redirect_url; when the browser cannot reach this
machine, paste the address it was sent to. The tokens are kept in
<state>/.mcp-oauth/<category>/<name>.json and refreshed by the runs that use the server.

--timeout bounds the wait for the sign-in (default: cli.mcp_login_timeout_ms of the root
setting, 10m unless set).

logout removes those tokens. --root picks the session root when more than one has the file.
`

// mcpServerFile is one MCP server file of a root's catalog layout and where its tokens are kept.
type mcpServerFile struct {
	root      string
	path      string
	tokenPath string
}

func runMCPCommand(args []string, streams commandStreams) error {
	if len(args) == 0 || (args[0] != "login" && args[0] != "logout") {
		if len(args) > 0 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help") {
			_, _ = io.WriteString(streams.stdout, mcpUsage)
			return nil
		}
		return usageError("%s", mcpUsage)
	}
	action := args[0]
	flags := flag.NewFlagSet("jad mcp "+action, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	root := flags.String("root", "", "session root ID")
	timeout := time.Duration(0)
	if action == "login" {
		flags.DurationVar(&timeout, "timeout", 0, "how long to wait for the sign-in (default: cli.mcp_login_timeout_ms)")
	}
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, _ = io.WriteString(streams.stdout, mcpUsage)
			return nil
		}
		return usageError("%v\n\n%s", err, mcpUsage)
	}
	if flags.NArg() != 1 {
		return usageError("name one MCP server file as <category>/<name>\n\n%s", mcpUsage)
	}
	given := flagGiven(flags, "timeout")
	if action == "login" && given && timeout <= 0 {
		return usageError("--timeout must be positive")
	}
	loaded, err := sessionfs.Load(sessionfs.LoadOptions{})
	if err != nil {
		return fmt.Errorf("load server configuration: %w", err)
	}
	if !given {
		timeout = loaded.CLI.MCPLoginTimeout
	}
	server, err := findMCPServerFile(loaded.Roots, flags.Arg(0), *root)
	if err != nil {
		return &exitError{code: exitFailed, err: err}
	}
	ctx, stop := interruptContext()
	defer stop()
	if action == "logout" {
		removed, err := mcpclient.Logout(ctx, server.tokenPath)
		if err != nil {
			return &exitError{code: exitFailed, err: err}
		}
		if removed {
			_, _ = fmt.Fprintf(streams.stdout, "Removed %s\n", server.tokenPath)
		} else {
			_, _ = fmt.Fprintf(streams.stdout, "No tokens were stored at %s\n", server.tokenPath)
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	err = mcpclient.Login(ctx, server.path, server.tokenPath, mcpclient.LoginOptions{
		In:          streams.stdin,
		Out:         streams.stderr,
		Environment: processEnvironment(),
	})
	if err != nil {
		return &exitError{code: exitFailed, err: err}
	}
	return nil
}

// findMCPServerFile finds mcp/<id>.json in the catalog layouts of the roots. Roots that
// share one layout name the same file once; a file in more than one layout needs --root.
func findMCPServerFile(roots []sessionfs.Root, id, rootID string) (mcpServerFile, error) {
	found := map[string]mcpServerFile{}
	known := false
	for _, root := range roots {
		if root.Layout == nil || (rootID != "" && root.ID != rootID) {
			continue
		}
		known = true
		servers, err := catalog.Scan("mcp", root.Layout.MCP)
		if err != nil {
			return mcpServerFile{}, err
		}
		entry, err := servers.Lookup(id)
		if err != nil {
			continue
		}
		file := mcpServerFile{root: root.ID, path: entry.Path, tokenPath: sessionfs.MCPTokenPath(*root.Layout, entry.ID())}
		key := file.path + "\x00" + file.tokenPath
		if _, seen := found[key]; !seen {
			found[key] = file
		}
	}
	if !known {
		if rootID != "" {
			return mcpServerFile{}, fmt.Errorf("no session root %q uses the catalog layout", rootID)
		}
		return mcpServerFile{}, errors.New("no session root uses the catalog layout")
	}
	switch len(found) {
	case 0:
		return mcpServerFile{}, fmt.Errorf("no MCP server file %s.json under mcp/", id)
	case 1:
		for _, file := range found {
			return file, nil
		}
	}
	ids := make([]string, 0, len(found))
	for _, file := range found {
		ids = append(ids, file.root)
	}
	sort.Strings(ids)
	return mcpServerFile{}, fmt.Errorf("%s is in the layouts of roots %s; pick one with --root", id, strings.Join(ids, ", "))
}

func processEnvironment() map[string]string {
	environment := map[string]string{}
	for _, entry := range os.Environ() {
		if name, value, ok := strings.Cut(entry, "="); ok {
			environment[name] = value
		}
	}
	return environment
}
