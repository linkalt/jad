package sessionfs

import (
	"encoding/json"
	"fmt"
	"strings"

	"example.com/agent-harness/backend-go/internal/catalog"
)

// ReadLayoutProfileInfo returns the profiles a catalog-layout session offers and its
// active profile, both as <category>/<name> IDs.
func ReadLayoutProfileInfo(location Location) (PathProfileInfo, error) {
	layout := *location.Root.Layout
	session, err := readLayoutSession(location)
	if err != nil {
		return PathProfileInfo{}, err
	}
	profiles, err := scopedProfiles(layout, session, location.SettingFile())
	if err != nil {
		return PathProfileInfo{}, err
	}
	return PathProfileInfo{ActiveProfile: session.ActiveProfile, PathProfiles: profiles.IDs()}, nil
}

func readLayoutSession(location Location) (layoutSessionFile, error) {
	var session layoutSessionFile
	if err := readLayoutFile(location.SettingFile(), &session); err != nil {
		return layoutSessionFile{}, err
	}
	return session, nil
}

func scopedProfiles(layout Layout, session layoutSessionFile, sessionPath string) (*catalog.Catalog, error) {
	profiles, err := catalog.Scan("profile", layout.Profiles)
	if err != nil {
		return nil, configError("%s: %v", sessionPath, err)
	}
	if profiles, err = profiles.Narrow(session.Scope.Profiles); err != nil {
		return nil, configError("%s: scope.profiles: %v", sessionPath, err)
	}
	return profiles, nil
}

// SetLayoutActiveProfile makes profile the session's active profile in session.json.
func SetLayoutActiveProfile(location Location, profile string, durability string) (PathProfileUpdate, error) {
	layout := *location.Root.Layout
	sessionPath := location.SettingFile()
	setting, err := readSessionObject(sessionPath, false)
	if err != nil {
		return PathProfileUpdate{}, err
	}
	session, err := readLayoutSession(location)
	if err != nil {
		return PathProfileUpdate{}, err
	}
	profiles, err := scopedProfiles(layout, session, sessionPath)
	if err != nil {
		return PathProfileUpdate{}, err
	}
	profile = strings.TrimSpace(profile)
	if _, found := profiles.Get(profile); !found {
		return PathProfileUpdate{}, &ClientError{Message: fmt.Sprintf("profile '%s' is not available to this session; available: %s", profile, strings.Join(profiles.IDs(), ", "))}
	}
	changed := session.ActiveProfile != profile
	if changed {
		if setting["active_profile"], err = json.Marshal(profile); err != nil {
			return PathProfileUpdate{}, err
		}
		if err := writeSessionSettingsAtomic(sessionPath, setting, durability == "strict"); err != nil {
			return PathProfileUpdate{}, fmt.Errorf("write session profile: %w", err)
		}
	}
	return PathProfileUpdate{ActiveProfile: profile, PathProfiles: profiles.IDs(), Changed: changed}, nil
}

// SetLayoutModelDefault records, in session.json, the model the session uses for a
// profile. The profile file is shared by every session, so it is never written.
func SetLayoutModelDefault(location Location, profile, model string, durability string) (ModelDefaultUpdate, error) {
	layout := *location.Root.Layout
	sessionPath := location.SettingFile()
	model = strings.TrimSpace(model)
	if model == "" {
		return ModelDefaultUpdate{}, &ClientError{Message: "model_default must be a non-empty string"}
	}
	setting, err := readSessionObject(sessionPath, false)
	if err != nil {
		return ModelDefaultUpdate{}, err
	}
	session, err := readLayoutSession(location)
	if err != nil {
		return ModelDefaultUpdate{}, err
	}
	profiles, err := scopedProfiles(layout, session, sessionPath)
	if err != nil {
		return ModelDefaultUpdate{}, err
	}
	if _, found := profiles.Get(profile); !found {
		return ModelDefaultUpdate{}, &ClientError{Message: fmt.Sprintf("profile '%s' is not available to this session", profile)}
	}
	chosen := make(map[string]string, len(session.ModelDefaults)+1)
	for key, value := range session.ModelDefaults {
		chosen[key] = value
	}
	changed := chosen[profile] != model
	chosen[profile] = model
	if changed {
		if setting["model_defaults"], err = json.Marshal(chosen); err != nil {
			return ModelDefaultUpdate{}, err
		}
		if err := writeSessionSettingsAtomic(sessionPath, setting, durability == "strict"); err != nil {
			return ModelDefaultUpdate{}, fmt.Errorf("write session model settings: %w", err)
		}
	}
	return ModelDefaultUpdate{ActiveProfile: profile, PathProfiles: profiles.IDs(), ModelDefault: model, Changed: changed}, nil
}
