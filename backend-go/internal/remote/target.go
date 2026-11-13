package remote

import (
	"fmt"
	"strings"
)

// Prefix starts every remote reference: remote:<name>/...
const Prefix = "remote:"

// Target is a parsed remote reference.
type Target struct {
	Remote, Session, Profile string
	// Agent is set for a sub-agent target.
	Agent string
	// Workspace is the binding's workspace (workspace layout); empty for a v1 reference, which
	// writes the session as the server takes it. WireSession is what the server is sent.
	Workspace string
}

// WireSession is the session as the remote server's routes take it: <workspace>:<session> for a
// binding of the workspace layout, the session as written for a v1 reference.
func (t Target) WireSession() string {
	if t.Workspace == "" {
		return t.Session
	}
	return t.Workspace + ":" + t.Session
}

// BindingForms is the sentence that lists the remote forms of the workspace layout.
const BindingForms = "remote:<binding>/<session>/<category>/<name>"

// ParseBindingProfile reads remote:<binding>/<session>/<category>/<profile> of the workspace layout:
// exactly four segments after the prefix, no default session and no <workspace>: form (the binding
// is the workspace). entry reads the binding for its workspace.
func ParseBindingProfile(s string, entry func(name string) (Entry, error)) (Target, error) {
	return parseBinding(s, 2, entry)
}

// ParseBindingSubagent is ParseBindingProfile with an agent: remote:<binding>/<session>/<category>/<profile>/<agent>.
func ParseBindingSubagent(s string, entry func(name string) (Entry, error)) (Target, error) {
	return parseBinding(s, 3, entry)
}

func parseBinding(s string, tail int, entry func(string) (Entry, error)) (Target, error) {
	shape := BindingForms
	if tail == 3 {
		shape += "/<agent>"
	}
	name, rest, ok := Split(s)
	if !ok || !NamePattern.MatchString(name) {
		return Target{}, fmt.Errorf("a remote reference is %s", shape)
	}
	parts := strings.Split(rest, "/")
	if len(parts) != tail+1 || !SessionPattern.MatchString(parts[0]) || !segmentsOK(parts[1:]...) {
		return Target{}, fmt.Errorf("a remote reference is %s with plain names (letters, digits, '_' and '-'; the session may also have '.')", shape)
	}
	found, err := entry(name)
	if err != nil {
		return Target{}, err
	}
	target := Target{Remote: name, Session: parts[0], Profile: parts[1] + "/" + parts[2], Workspace: found.Workspace}
	if tail == 3 {
		target.Agent = parts[3]
	}
	return target, nil
}

// Split cuts "remote:<name>/<rest>" into the name and the rest. ok is false when s is not a
// remote reference at all (no prefix), so callers can fall through to local handling.
func Split(s string) (name, rest string, ok bool) {
	body, found := strings.CutPrefix(s, Prefix)
	if !found {
		return "", "", false
	}
	name, rest, _ = strings.Cut(body, "/")
	return name, rest, true
}

func segmentsOK(parts ...string) bool {
	for _, part := range parts {
		if !NamePattern.MatchString(part) {
			return false
		}
	}
	return true
}

// ParseProfile reads remote:<name>/<session>/<category>/<profile>, or
// remote:<name>/<category>/<profile> when the entry has a default_session.
func ParseProfile(s string, entry func(name string) (Entry, error)) (Target, error) {
	name, rest, ok := Split(s)
	if !ok {
		return Target{}, fmt.Errorf("%q is not a remote reference", s)
	}
	return parseRest(name, rest, 2, false, entry)
}

// ParseSubagent reads remote:<name>/<session>/<category>/<profile>/<agent>, or without the
// session when the entry has a default_session.
func ParseSubagent(s string, entry func(name string) (Entry, error)) (Target, error) {
	name, rest, ok := Split(s)
	if !ok {
		return Target{}, fmt.Errorf("%q is not a remote reference", s)
	}
	return parseRest(name, rest, 3, true, entry)
}

func parseRest(name, rest string, withoutSession int, agent bool, entry func(string) (Entry, error)) (Target, error) {
	shape := "remote:<name>/[<workspace>:]<session>/<category>/<profile>"
	if agent {
		shape += "/<agent>"
	}
	if !NamePattern.MatchString(name) {
		return Target{}, fmt.Errorf("a remote reference is %s", shape)
	}
	parts := strings.Split(rest, "/")
	target := Target{Remote: name}
	switch len(parts) {
	case withoutSession + 1:
		if !SessionRefPattern.MatchString(parts[0]) {
			return Target{}, fmt.Errorf("a remote reference is %s", shape)
		}
		target.Session, parts = parts[0], parts[1:]
	case withoutSession:
		found, err := entry(name)
		if err != nil {
			return Target{}, err
		}
		if found.DefaultSession == "" {
			return Target{}, fmt.Errorf("remote %s has no default_session, so the reference must name the session: %s", name, shape)
		}
		target.Session = found.DefaultSession
	default:
		return Target{}, fmt.Errorf("a remote reference is %s", shape)
	}
	if !segmentsOK(parts...) {
		return Target{}, fmt.Errorf("a remote reference is %s with plain names (letters, digits, '_' and '-')", shape)
	}
	target.Profile = parts[0] + "/" + parts[1]
	if agent {
		target.Agent = parts[2]
	}
	return target, nil
}

// Scope is the category/name (and agent) the entry's profiles globs are matched against.
func (t Target) Scope() string {
	if t.Agent != "" {
		return t.Profile + "/" + t.Agent
	}
	return t.Profile
}

// String is the reference as a model writes it.
func (t Target) String() string {
	text := Prefix + t.Remote + "/" + t.Session + "/" + t.Profile
	if t.Agent != "" {
		text += "/" + t.Agent
	}
	return text
}
