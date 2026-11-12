package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools/native"
)

// Profile implements native.TranscriptProfiles: it finds the transcript files of a profile of a
// session without loading the profile or creating a file. An empty session is the caller's own
// and an empty profile is the caller's profile.
func (m profileMessenger) Profile(ctx context.Context, ownerKey, session, profile string) (native.TranscriptTarget, error) {
	callerSession, callerProfile, found := strings.Cut(ownerKey, "\x00")
	if !found {
		return native.TranscriptTarget{}, errors.New("transcript needs the calling session key")
	}
	if session == "" {
		session = callerSession
	}
	if profile == "" {
		profile = callerProfile
	}
	location, err := m.resolveSession(ownerKey, callerSession, session)
	if err != nil {
		return native.TranscriptTarget{}, mapSessionError(err)
	}
	messages, ok := location.ProfileMessagesPath(profile)
	if !ok {
		return native.TranscriptTarget{}, fmt.Errorf("profile %q is not a <category>/<name> profile of a catalog session", profile)
	}
	config, err := sessionfs.LoadSessionSettings(location, profile, "")
	if err != nil {
		return native.TranscriptTarget{}, mapSessionError(err)
	}
	target := native.TranscriptTarget{
		Session: location.Reference,
		Profile: config.Profile,
		Files:   native.TranscriptFiles{Messages: messages, Backup: config.StoragePaths.MessagesBackup},
	}
	if config.NativeSubagent != nil {
		target.SubagentDir = config.NativeSubagent.StorageDir
		target.PersistRuns = config.NativeSubagent.PersistRuns
		target.Runs = m.resolver.existingSubagentManager(target.SubagentDir)
	}
	return target, nil
}

// resolveSession finds the session a transcript tool names as its caller sees it: a session
// of the caller's workspace (sessionfs.ResolveFrom), or, for the remote API's reader, any
// workspace of the server, as the token's globs allow (a session that does not exist fails
// where its file is read). The reader is told by its whole owner
// key, which no session's key can equal (a profile id has a slash), not by a session id.
func (m profileMessenger) resolveSession(ownerKey, callerSession, session string) (sessionfs.Location, error) {
	if ownerKey == remoteTranscriptCaller {
		return sessionfs.Resolve(m.resolver.roots, session)
	}
	return sessionfs.ResolveFrom(m.resolver.roots, callerSession, session)
}

// Candidates implements native.TranscriptProfiles: the profiles of the session, or the
// sessions when that session cannot be read.
func (m profileMessenger) Candidates(ctx context.Context, ownerKey, session string) (profiles, sessions []string) {
	callerSession, _, _ := strings.Cut(ownerKey, "\x00")
	if session == "" {
		session = callerSession
	}
	if location, err := m.resolveSession(ownerKey, callerSession, session); err == nil && location.UsesLayout() {
		if info, err := sessionfs.ReadLayoutProfileInfo(location); err == nil {
			return info.Profiles, nil
		}
	}
	return nil, m.Sessions(ctx, ownerKey)
}

// Sessions implements native.TranscriptSessions: the references of the catalog sessions the
// caller may see (in the workspace layout, those of its own workspace).
func (m profileMessenger) Sessions(ctx context.Context, ownerKey string) []string {
	callerSession, _, _ := strings.Cut(ownerKey, "\x00")
	files, _ := sessionfs.ListSessionFiles(m.resolver.roots)
	sessions := make([]string, 0, len(files))
	for _, file := range files {
		sessions = append(sessions, file.ID)
	}
	return sessionfs.VisibleFrom(m.resolver.roots, callerSession, sessions)
}
