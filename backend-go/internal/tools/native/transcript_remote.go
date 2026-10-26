package native

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"jad/backend-go/internal/remote"
	"jad/backend-go/internal/tools"
)

// remoteReader is what transcript and transcripts know about remotes: whether the switch is on,
// the remote entries they may use and the folder those live in.
type remoteReader struct {
	allow     bool
	names     []string
	directory *remote.Directory
}

// transcriptRemoteKeys are the inputs of transcript that are forwarded to the remote's own tool.
var transcriptRemoteKeys = []string{"turns", "source", "role", "tool", "search", "context", "step", "part"}

// transcriptsRemoteKeys are the same for transcripts.
var transcriptsRemoteKeys = []string{"status", "limit"}

func (r remoteReader) permits(name string) bool {
	for _, allowed := range r.names {
		if allowed == name {
			return true
		}
	}
	return false
}

// remoteRead is one call to a remote: the tool to run there, the remote's name and what to ask.
type remoteRead struct {
	tool, name string
	query      url.Values
}

// remoteShapes are the forms of a remote path per tool: kind, parts without the session (the
// entry's default_session fills it) and parts with it. A path: is never read on a remote.
var remoteShapes = map[string]map[string][2]int{
	"transcript":  {"profile": {2, 3}, "subagent": {3, 4}},
	"transcripts": {"profile": {2, 3}, "session": {1, 1}},
}

const remotePathForms = "remote:<name>/profile:<session>/<category>/<name>, remote:<name>/subagent:<session>/<category>/<name>/<run id>"

// selector turns "remote:<name>/<rest>" into the name and the remote's own path, after the
// switch, the allow-list of this tool and the entry's profile globs. An empty rest lists the
// default session (transcripts only).
func (r remoteReader) selector(tool, reference string) (path string, failure *remoteFailure) {
	invalid := func(text string) (string, *remoteFailure) {
		return "", &remoteFailure{status: tools.StatusInvalidInput, code: "bad_remote_reference", text: text}
	}
	name, rest, _ := remote.Split(reference)
	if !remote.NamePattern.MatchString(name) {
		return invalid("a remote path is " + remotePathForms)
	}
	if failure := r.check(name); failure != nil {
		return "", failure
	}
	entry, err := r.directory.Entry(name)
	if err != nil {
		return "", &remoteFailure{status: tools.StatusError, code: "remote_unavailable", text: remoteProblem(name, err)}
	}
	if rest == "" {
		if tool != "transcripts" || entry.DefaultSession == "" {
			return invalid(fmt.Sprintf("name what to read on remote %s: %s (remote:%s alone lists its default_session, when it has one)", name, remotePathForms, name))
		}
		return "session:" + entry.DefaultSession, nil
	}
	kind, body, found := strings.Cut(rest, ":")
	shape, known := remoteShapes[tool][kind]
	parts := strings.Split(body, "/")
	if !found || !known || len(parts) < shape[0] || len(parts) > shape[1] {
		return invalid("a remote path is " + remotePathForms + "; path: is never read on a remote")
	}
	if len(parts) == shape[0] && shape[0] != shape[1] {
		if entry.DefaultSession == "" {
			return invalid(fmt.Sprintf("remote %s has no default_session, so the path must name the session", name))
		}
		parts = append([]string{entry.DefaultSession}, parts...)
	}
	for index, part := range parts {
		valid := remote.NamePattern.MatchString(part)
		switch {
		case index == 0:
			valid = remote.SessionPattern.MatchString(part)
		case kind == "subagent" && index == 3:
			valid = transcriptRunID.MatchString(part)
		}
		if !valid {
			return invalid("a remote path is " + remotePathForms + " with plain names")
		}
	}
	if kind != "session" && !entry.Allows(parts[1]+"/"+parts[2]) {
		return "", &remoteFailure{status: tools.StatusForbidden, code: "remote_not_allowed", text: fmt.Sprintf("Profile %s/%s is not among the profiles this session may reach on remote %s", parts[1], parts[2], name)}
	}
	return kind + ":" + strings.Join(parts, "/"), nil
}

// check refuses a remote the switch or the allow-list does not give.
func (r remoteReader) check(name string) *remoteFailure {
	if !r.allow || len(r.names) == 0 || r.directory == nil {
		return &remoteFailure{status: tools.StatusForbidden, code: "remote_disabled", text: "Reading another JAD server is off: set allow_remote and remotes in this tool's settings"}
	}
	if !r.permits(name) {
		return &remoteFailure{status: tools.StatusInvalidInput, code: "unknown_remote", text: fmt.Sprintf("Remote %s is not available to this tool (available: %s)", name, strings.Join(r.names, ", "))}
	}
	return nil
}

// remoteFailure is a rendered-to-be failure of a remote read; it never carries a secret.
type remoteFailure struct {
	status tools.Status
	code   string
	text   string
}

func (f *remoteFailure) render(callID string) string {
	return tools.ExecutionError(callID, f.status, f.code, f.text)
}

// remoteCursor is the cursor text a remote's cursor is wrapped in, so the next call knows where
// to send it: remote:<name>/<cursor>.
func remoteCursor(name, cursor string) string { return remote.Prefix + name + "/" + cursor }

// splitRemoteCursor reads a wrapped cursor; ok is false when text is not one.
func splitRemoteCursor(text string) (name, cursor string, ok bool) {
	name, rest, found := remote.Split(text)
	if !found || rest == "" || !remote.NamePattern.MatchString(name) || strings.ContainsAny(rest, "/:") {
		return "", "", false
	}
	return name, rest, true
}

// read runs the call on the remote and returns its rendered result with the paths and cursors
// made usable here.
func (r remoteReader) read(ctx context.Context, callID string, call remoteRead, hops, contentMax int) string {
	client, err := r.directory.Client(call.name)
	if err != nil {
		return tools.ExecutionError(callID, tools.StatusError, "remote_unavailable", remoteProblem(call.name, err))
	}
	output, err := client.ReadTranscript(ctx, call.tool, call.query, hops)
	if err != nil {
		return remoteReadError(callID, call.name, err)
	}
	return rewriteRemoteOutput(output, call.name, contentMax)
}

func remoteReadError(callID, name string, err error) string {
	var failure *remote.Error
	switch {
	case errors.As(err, &failure):
		switch {
		case failure.Code == "forbidden" || failure.Status == http.StatusForbidden:
			return tools.ExecutionError(callID, tools.StatusForbidden, "remote_forbidden", fmt.Sprintf("Remote %s refused: its token may not read transcripts there", name))
		case failure.Code == "unauthorized" || failure.Status == http.StatusUnauthorized:
			return tools.ExecutionError(callID, tools.StatusForbidden, "remote_unauthorized", fmt.Sprintf("Remote %s did not accept this server's token", name))
		case failure.Code == "remote_hop_limit":
			return tools.ExecutionError(callID, tools.StatusError, "remote_hop_limit", fmt.Sprintf("Remote %s refused: the request has passed through too many JAD servers", name))
		case failure.Code == "no_session":
			return tools.ExecutionError(callID, tools.StatusNotFound, "no_such_session", fmt.Sprintf("Remote %s has no such session", name))
		}
		return tools.ExecutionError(callID, tools.StatusError, "remote_failed", failure.Error())
	case errors.Is(err, remote.ErrRedirect), errors.Is(err, remote.ErrResponseTooLarge):
		return tools.ExecutionError(callID, tools.StatusError, "remote_failed", err.Error())
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return tools.ExecutionError(callID, tools.StatusError, "remote_timeout", fmt.Sprintf("Reading from remote %s took too long or was cancelled", name))
	}
	return tools.ExecutionError(callID, tools.StatusError, "remote_unreachable", err.Error())
}

// rewriteRemoteOutput makes what the remote's tool printed usable here: the paths a line gives
// and the cursor get the remote:<name>/ prefix, and the content is cut to contentMax bytes.
// The text is the remote's and is only data; just these line shapes are touched.
func rewriteRemoteOutput(output, name string, contentMax int) string {
	prefix := remote.Prefix + name + "/"
	head, content, hasContent := strings.Cut(output, "\n--- CONTENT ")
	lines := strings.Split(head, "\n")
	for i, line := range lines {
		switch {
		case strings.HasPrefix(line, "cursor: "):
			lines[i] = "cursor: " + remoteCursor(name, strings.TrimPrefix(line, "cursor: "))
		case i == 0 && strings.HasPrefix(line, `{"id":`):
			for _, kind := range []string{"profile:", "subagent:", "session:"} {
				lines[i] = strings.Replace(lines[i], `"summary":"`+kind, `"summary":"`+prefix+kind, 1)
			}
		}
	}
	head = strings.Join(lines, "\n")
	if !hasContent {
		return head
	}
	end := strings.LastIndex(content, "\n--- END CONTENT ")
	if end < 0 {
		return head + "\n--- CONTENT " + content
	}
	body, tail := content[:end], content[end+len("\n--- END CONTENT "):]
	label, text, _ := strings.Cut(body, "\n")
	rows := strings.Split(text, "\n")
	for i, row := range rows {
		if before, after, found := strings.Cut(row, " | path "); found {
			path, rest, more := strings.Cut(after, " | ")
			for _, kind := range []string{"profile:", "subagent:", "session:"} {
				if strings.HasPrefix(path, kind) {
					row = before + " | path " + prefix + path
					if more {
						row += " | " + rest
					}
					rows[i] = row
					break
				}
			}
		}
	}
	text = strings.Join(rows, "\n")
	if contentMax > 0 && len(text) > contentMax {
		text = midCut(text, contentMax)
	}
	return head + "\n--- CONTENT " + label + "\n" + text + "\n--- END CONTENT " + tail
}

// forwardRemoteArguments copies the listed keys of arguments into a query. Values are strings
// or integers by the tool's schema; anything else cannot get here.
func forwardRemoteArguments(arguments map[string]any, keys []string, query url.Values) {
	for _, key := range keys {
		switch value := arguments[key].(type) {
		case string:
			query.Set(key, value)
		case json.Number:
			query.Set(key, value.String())
		case float64:
			query.Set(key, strconv.FormatInt(int64(value), 10))
		}
	}
}

// executeRemote reads a transcript of a remote: the remote runs its own transcript tool and the
// answer comes back rendered, with the paths and the cursor prefixed remote:<name>/.
func (t *transcriptTool) executeRemote(ctx context.Context, arguments map[string]any, execution tools.ExecutionContext, name string) string {
	query := url.Values{}
	if cursor, has := arguments["cursor"].(string); has {
		wrapped, inner, ok := splitRemoteCursor(cursor)
		if !ok || wrapped != name {
			return t.fail(execution, tools.StatusInvalidInput, "invalid_cursor", "cursor is not a cursor this tool gave")
		}
		for _, key := range []string{"path", "turns", "step", "part"} {
			if _, given := arguments[key]; given {
				return t.fail(execution, tools.StatusInvalidInput, "conflicting_selectors", "A cursor already names its transcript; give only the cursor")
			}
		}
		if failure := t.remotes.check(name); failure != nil {
			return failure.render(execution.CallID)
		}
		query.Set("cursor", inner)
		if source, ok := arguments["source"].(string); ok {
			query.Set("source", source)
		}
	} else {
		selector, _ := arguments["path"].(string)
		path, failure := t.remotes.selector("transcript", selector)
		if failure != nil {
			return failure.render(execution.CallID)
		}
		query.Set("path", path)
		forwardRemoteArguments(arguments, transcriptRemoteKeys, query)
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(t.settings.TimeoutMs)*time.Millisecond)
	defer cancel()
	return t.remotes.read(ctx, execution.CallID, remoteRead{tool: "transcript", name: name, query: query}, execution.RemoteHops+1, int(t.settings.ResponseMaxBytes))
}

func stringArgument(arguments map[string]any, key string) string {
	text, _ := arguments[key].(string)
	return text
}

// executeRemote lists on a remote: its own transcripts tool runs there, with the paths hidden,
// and the answer comes back with every path and the cursor prefixed remote:<name>/.
func (t *transcriptsTool) executeRemote(ctx context.Context, arguments map[string]any, execution tools.ExecutionContext, name string) string {
	if execution.AgentDepth > 0 && !t.settings.AllowChildren {
		return t.fail(execution, tools.StatusForbidden, "transcripts_children_denied", "A sub-agent may not list transcripts: tools.native.transcripts.allow_children is false")
	}
	query := url.Values{}
	if cursor, has := arguments["cursor"].(string); has {
		wrapped, inner, ok := splitRemoteCursor(cursor)
		if !ok || wrapped != name {
			return t.fail(execution, tools.StatusInvalidInput, "invalid_cursor", "cursor is not a cursor this tool gave")
		}
		for _, key := range []string{"path", "filters", "status"} {
			if _, given := arguments[key]; given {
				return t.fail(execution, tools.StatusInvalidInput, "conflicting_selectors", "A cursor already names its listing, filters and status; give only the cursor (and limit)")
			}
		}
		if failure := t.remotes.check(name); failure != nil {
			return failure.render(execution.CallID)
		}
		query.Set("cursor", inner)
	} else {
		path, failure := t.remotes.selector("transcripts", stringArgument(arguments, "path"))
		if failure != nil {
			return failure.render(execution.CallID)
		}
		query.Set("path", path)
		if filters, ok := arguments["filters"].([]any); ok {
			for _, filter := range filters {
				if text, ok := filter.(string); ok && strings.TrimSpace(text) != "" {
					query.Add("filters", text)
				}
			}
		}
	}
	forwardRemoteArguments(arguments, transcriptsRemoteKeys[:], query)
	if _, has := arguments["cursor"]; has {
		query.Del("status")
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(t.settings.TimeoutMs)*time.Millisecond)
	defer cancel()
	return t.remotes.read(ctx, execution.CallID, remoteRead{tool: "transcripts", name: name, query: query}, execution.RemoteHops+1, 0)
}
