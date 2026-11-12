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
	location, err := sessionfs.ResolveFrom(m.resolver.roots, callerSession, session)
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

// Candidates implements native.TranscriptProfiles: the profiles of the session, or the
// sessions when that session cannot be read.
func (m profileMessenger) Candidates(ctx context.Context, ownerKey, session string) (profiles, sessions []string) {
	callerSession, _, _ := strings.Cut(ownerKey, "\x00")
	if session == "" {
		session = callerSession
	}
	if location, err := sessionfs.ResolveFrom(m.resolver.roots, callerSession, session); err == nil && location.UsesLayout() {
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
