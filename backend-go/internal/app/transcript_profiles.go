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
	location, err := sessionfs.Resolve(m.resolver.roots, session)
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
	return native.TranscriptTarget{
		Session: location.Reference,
		Profile: config.Profile,
		Files:   native.TranscriptFiles{Messages: messages, Backup: config.StoragePaths.MessagesBackup},
	}, nil
}
