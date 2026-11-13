package sessionfs

import (
	"errors"
	"regexp"
	"strings"
)

// The profile_message address grammar of the workspace layout (v2; docs/design_workspace_layout.md
// §30, tmp/decider_handoff.md §2). A profile is written in one of five forms with a fixed
// number of '/' separated segments; a colon appears only after the local: or remote: prefix.
//
//	name                                         the caller's workspace, session and category
//	category/name                                the caller's workspace and session
//	session/category/name                        the caller's workspace
//	local:workspace/session/category/name        a listed workspace of this process
//	remote:binding/session/category/name         a remote binding
//
// Nothing is searched or defaulted: a form that does not name a segment never guesses it.

const (
	// LocalPrefix starts a local address that names its workspace.
	LocalPrefix = "local:"
	// RemotePrefix starts a remote address.
	RemotePrefix = "remote:"
)

// MessageCaller is who writes an address: the workspace, session and profile (<category>/<name>)
// of the calling run. Its Workspace is empty outside the workspace layout, which has no address
// grammar of its own.
type MessageCaller struct {
	Workspace string
	Session   string
	Profile   string
}

// MessageRef is a parsed local profile_message address, always fully qualified.
type MessageRef struct {
	Workspace string
	Session   string
	// Profile is <category>/<name>.
	Profile string
}

// Canonical is the one written form of a local address: local:<workspace>/<session>/<category>/<name>.
// Results, labels, chain entries and deny rules all use it.
func (r MessageRef) Canonical() string {
	return LocalPrefix + r.Workspace + "/" + r.Session + "/" + r.Profile
}

// Own reports whether the address names a profile of the caller's own session.
func (r MessageRef) Own(caller MessageCaller) bool {
	return r.Workspace == caller.Workspace && r.Session == caller.Session
}

// Reference is the session reference the address names for the roots: bare for a session of the
// caller's workspace and <workspace>:<session> for another (what ResolveMessageTarget takes).
func (r MessageRef) Reference(caller MessageCaller) string {
	if r.Workspace == caller.Workspace {
		return r.Session
	}
	return r.Workspace + ":" + r.Session
}

// ErrInvalidMessageRef is a profile address that is none of the valid forms.
var ErrInvalidMessageRef = errors.New("not a valid profile address")

// MessageRefForms is the sentence the tool and its errors use to list the valid forms.
const MessageRefForms = "<name>, <category>/<name>, <session>/<category>/<name>, local:<workspace>/<session>/<category>/<name> or remote:<binding>/<session>/<category>/<name>"

var segmentPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// ParseMessageRef reads a local address written by caller. A remote address (the remote:
// prefix) is not read here: callers split it off first.
func ParseMessageRef(reference string, caller MessageCaller) (MessageRef, error) {
	if reference == "" || strings.HasPrefix(reference, RemotePrefix) {
		return MessageRef{}, ErrInvalidMessageRef
	}
	qualified := false
	body := reference
	if rest, found := strings.CutPrefix(reference, LocalPrefix); found {
		qualified, body = true, rest
	}
	if strings.Contains(body, ":") {
		return MessageRef{}, ErrInvalidMessageRef
	}
	segments := strings.Split(body, "/")
	for _, segment := range segments {
		if !segmentPattern.MatchString(segment) {
			return MessageRef{}, ErrInvalidMessageRef
		}
	}
	callerCategory, _, _ := strings.Cut(caller.Profile, "/")
	var ref MessageRef
	switch {
	case qualified && len(segments) == 4:
		ref = MessageRef{Workspace: segments[0], Session: segments[1], Profile: segments[2] + "/" + segments[3]}
	case qualified:
		return MessageRef{}, ErrInvalidMessageRef
	case len(segments) == 1:
		if callerCategory == "" {
			return MessageRef{}, ErrInvalidMessageRef
		}
		ref = MessageRef{Workspace: caller.Workspace, Session: caller.Session, Profile: callerCategory + "/" + segments[0]}
	case len(segments) == 2:
		ref = MessageRef{Workspace: caller.Workspace, Session: caller.Session, Profile: body}
	case len(segments) == 3:
		ref = MessageRef{Workspace: caller.Workspace, Session: segments[0], Profile: segments[1] + "/" + segments[2]}
	default:
		return MessageRef{}, ErrInvalidMessageRef
	}
	if !rootIDPattern.MatchString(ref.Workspace) || !sessionIDPattern.MatchString(ref.Session) {
		return MessageRef{}, ErrInvalidMessageRef
	}
	return ref, nil
}

// messageCallerOf is the caller of a session at location in the workspace layout, without its
// profile (the profile is known only once the session's profile is selected); the zero value
// for a session of any other layout.
func messageCallerOf(location Location) MessageCaller {
	if layout := location.Root.Layout; layout == nil || layout.Workspace == "" {
		return MessageCaller{}
	}
	return MessageCaller{Workspace: location.Root.ID, Session: location.SessionID}
}
