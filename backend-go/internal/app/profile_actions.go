package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools/native"
)

// ProfileInfo implements native.ProfileActions: the input tokens of the profile's last completed
// response, or nil when no response has completed (or none carried usage). It reads the same
// transcript index the catalog reads, so it never builds a second store.
func (m profileMessenger) ProfileInfo(ctx context.Context, ownerKey, session, profile string) (*int64, error) {
	config, err := m.profileConfig(ownerKey, session, profile)
	if err != nil {
		return nil, err
	}
	shared, release, err := m.resolver.sharedStore(config)
	if err != nil {
		return nil, err
	}
	defer release()
	index, err := scanTranscriptIndex(shared, config)
	if err != nil {
		return nil, fmt.Errorf("read usage of profile %q: %w", profile, err)
	}
	if index.usage.Current == nil || index.usage.Current.InputTokens == nil {
		return nil, nil
	}
	tokens := *index.usage.Current.InputTokens
	return &tokens, nil
}

// CompactProfile implements native.ProfileActions through the same compaction the HTTP route
// uses (Resolver.CompactProfile), not profile_message's allow_other_profiles gate.
func (m profileMessenger) CompactProfile(ctx context.Context, ownerKey, session, profile string) error {
	reference, name, err := m.profileTarget(ownerKey, session, profile)
	if err != nil {
		return err
	}
	_, err = m.resolver.CompactProfile(ctx, reference, name)
	return err
}

// ResetProfile implements native.ProfileActions through ResetProfile, which refuses while a run
// of the profile is active.
func (m profileMessenger) ResetProfile(ctx context.Context, ownerKey, session, profile string) error {
	reference, name, err := m.profileTarget(ownerKey, session, profile)
	if err != nil {
		return err
	}
	return m.resolver.ResetProfile(ctx, reference, name)
}

// profileTarget resolves the session and profile a request names: an empty session is the
// caller's, which the messenger does not know here, so the session must be given. The session
// is looked up as the caller (ownerKey) sees it: in the workspace layout, in its own workspace.
func (m profileMessenger) profileTarget(ownerKey, session, profile string) (string, string, error) {
	if session == "" || profile == "" {
		return "", "", errors.New("profile actions need the session and the profile named")
	}
	callerSession, _, _ := strings.Cut(ownerKey, "\x00")
	location, err := sessionfs.ResolveFrom(m.resolver.roots, callerSession, session)
	if err != nil {
		return "", "", mapSessionError(err)
	}
	if !location.UsesLayout() {
		return "", "", native.ErrProfileUnknown
	}
	return location.Reference, strings.TrimSpace(profile), nil
}

// ProfileConfig implements native.ProfileActions: the configuration of the caller's own
// profile. The session and the profile are those of ownerKey, so no input can name another.
func (m profileMessenger) ProfileConfig(_ context.Context, ownerKey string, run sessionfs.ProfileRun) (sessionfs.ProfileConfig, error) {
	session, profile, found := strings.Cut(ownerKey, "\x00")
	if !found || session == "" || profile == "" {
		return sessionfs.ProfileConfig{}, errors.New("config needs the caller's session and profile")
	}
	config, err := m.profileConfig(ownerKey, session, profile)
	if err != nil {
		return sessionfs.ProfileConfig{}, err
	}
	return sessionfs.ReadProfileConfig(config, run)
}

// EditProfile implements native.ProfileActions: an edit or revert of the caller's own profile
// file. The session and the profile are those of ownerKey. No gate or run lock is taken: the
// edit comes from inside the caller's run, which holds them, and changes only the next run.
func (m profileMessenger) EditProfile(_ context.Context, ownerKey string, request sessionfs.ProfileEditRequest) (sessionfs.ProfileEditResult, error) {
	location, profile, err := m.ownProfile(ownerKey)
	if err != nil {
		return sessionfs.ProfileEditResult{}, err
	}
	request.Audit.Session = location.Reference
	return sessionfs.EditProfile(location, profile, request)
}

// ProfileHistory implements native.ProfileActions: the history of the caller's own profile file.
func (m profileMessenger) ProfileHistory(_ context.Context, ownerKey string, limit int) (sessionfs.ProfileHistory, error) {
	location, profile, err := m.ownProfile(ownerKey)
	if err != nil {
		return sessionfs.ProfileHistory{}, err
	}
	return sessionfs.ReadProfileHistory(location, profile, limit)
}

// ownProfile is the session location and profile of ownerKey, looked up as the caller sees it.
func (m profileMessenger) ownProfile(ownerKey string) (sessionfs.Location, string, error) {
	session, profile, found := strings.Cut(ownerKey, "\x00")
	if !found || session == "" || profile == "" {
		return sessionfs.Location{}, "", errors.New("a profile change needs the caller's session and profile")
	}
	reference, name, err := m.profileTarget(ownerKey, session, profile)
	if err != nil {
		return sessionfs.Location{}, "", err
	}
	location, err := sessionfs.Resolve(m.resolver.roots, reference)
	if err != nil {
		return sessionfs.Location{}, "", mapSessionError(err)
	}
	return location, name, nil
}

// profileConfig loads the settings of a profile of a session for a read.
func (m profileMessenger) profileConfig(ownerKey, session, profile string) (sessionfs.SessionConfig, error) {
	reference, name, err := m.profileTarget(ownerKey, session, profile)
	if err != nil {
		return sessionfs.SessionConfig{}, err
	}
	location, err := sessionfs.Resolve(m.resolver.roots, reference)
	if err != nil {
		return sessionfs.SessionConfig{}, mapSessionError(err)
	}
	config, err := sessionfs.LoadSessionSettings(location, name, "")
	if err != nil {
		return sessionfs.SessionConfig{}, mapSessionError(err)
	}
	return config, nil
}
