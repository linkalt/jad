package remoteapi

import (
	"regexp"
	"slices"
	"strings"
)

// A token row of the workspace layout reaches one workspace (Token.Workspace). The server limits
// each request to it before any backend is asked, so the Backend stays as it was: it is given
// <workspace>:<session>, which it already resolves to one root. A request that names another
// workspace, a workspace the server does not serve and a session that does not exist all answer
// as a missing session.

var bareSessionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// qualify turns a session as written in a request into its bare id (what the row's globs are
// matched against) and the reference the backend is given. A token without a workspace is a v1
// row: the session is passed as written. ok is false when the session is not in the row's workspace.
func (s *Server) qualify(token Token, session string) (bare, reference string, ok bool) {
	if token.Workspace == "" {
		return session, session, true
	}
	if !slices.Contains(s.opts.Workspaces, token.Workspace) {
		return "", "", false
	}
	if workspace, rest, qualified := strings.Cut(session, ":"); qualified {
		if workspace != token.Workspace {
			return "", "", false
		}
		session = rest
	}
	if !bareSessionPattern.MatchString(session) {
		return "", "", false
	}
	return session, token.Workspace + ":" + session, true
}

// may returns the token's profile and agent checks over the names the backend uses for a session,
// which are canonical references: <workspace>:<session> on a server of several workspaces and the
// bare id on one workspace alone. A row of the workspace layout reaches a name only inside its
// workspace, and matches its globs on the bare session; a bare name is the row's only when the
// server has no other workspace.
func (s *Server) may(token Token) (profile, agent func(string) bool) {
	if token.Workspace == "" {
		return token.MayProfile, token.MayAgent
	}
	inside := func(match func(string) bool) func(string) bool {
		return func(target string) bool {
			session, rest, found := strings.Cut(target, "/")
			if !found {
				return false
			}
			if workspace, bare, qualified := strings.Cut(session, ":"); qualified {
				if workspace != token.Workspace {
					return false
				}
				session = bare
			} else if len(s.opts.Workspaces) != 1 || s.opts.Workspaces[0] != token.Workspace {
				return false
			}
			return match(session + "/" + rest)
		}
	}
	return inside(token.MayProfile), inside(token.MayAgent)
}

// qualifyPath makes the session of a transcript path (profile:<session>/..., subagent:<session>/...
// or session:<session>) the reference the backend is given, for a row of the workspace layout;
// a path of any other shape is left for the backend to refuse. ok is false when the path names a
// session of another workspace.
func (s *Server) qualifyPath(token Token, path string) (string, bool) {
	if token.Workspace == "" {
		return path, true
	}
	kind, rest, found := strings.Cut(path, ":")
	if !found || (kind != "profile" && kind != "subagent" && kind != "session") {
		return path, true
	}
	session, tail, hasTail := strings.Cut(rest, "/")
	_, reference, ok := s.qualify(token, session)
	if !ok {
		return "", false
	}
	if hasTail {
		reference += "/" + tail
	}
	return kind + ":" + reference, true
}
