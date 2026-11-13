package contentlibrary

import (
	"context"
	"errors"
	"path/filepath"
	"sort"
)

// maxSkillNameLength is the longest skill name the library accepts, in UTF-16
// units like the TypeScript backend. The name pattern is ASCII, so it is also
// the byte length.
const maxSkillNameLength = 64

// LoadedSkill is one skill together with the directory it was loaded from.
// Relative files the skill mentions resolve against Directory.
type LoadedSkill struct {
	Asset     Asset
	Directory string
}

// ListSkills returns the skills visible across directories, in directory order.
// When two directories define the same name the earlier one wins, so a project
// can override a shared skill. The result is sorted by name.
func ListSkills(ctx context.Context, directories []string, maxBytes int64) ([]Asset, error) {
	if ctx == nil {
		return nil, errors.New("skill request context is required")
	}
	byName := make(map[string]Asset)
	for _, root := range directories {
		files, err := filesForKind(ctx, Skill, []string{root})
		if err != nil {
			return nil, err
		}
		for _, file := range files {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			asset, err := readAsset(Skill, file.root, file.path, maxBytes)
			if err != nil {
				return nil, err
			}
			if asset == nil {
				continue
			}
			if _, exists := byName[asset.Name]; !exists {
				byName[asset.Name] = *asset
			}
		}
	}
	skills := make([]Asset, 0, len(byName))
	for _, asset := range byName {
		skills = append(skills, asset)
	}
	sort.Slice(skills, func(i, j int) bool { return localeCompare(skills[i].Name, skills[j].Name) < 0 })
	return skills, nil
}

// ReadSkill returns one skill by name. A valid skill's folder name equals its
// name, so the file is addressed directly instead of scanning every skill; the
// name pattern rules out path separators and "..".
func ReadSkill(ctx context.Context, directories []string, name string, maxBytes int64) (LoadedSkill, bool, error) {
	if ctx == nil {
		return LoadedSkill{}, false, errors.New("skill request context is required")
	}
	if len(name) > maxSkillNameLength || !skillNamePattern.MatchString(name) {
		return LoadedSkill{}, false, nil
	}
	for _, root := range directories {
		if err := ctx.Err(); err != nil {
			return LoadedSkill{}, false, err
		}
		directory := filepath.Join(root, name)
		asset, err := readAsset(Skill, root, filepath.Join(directory, "SKILL.md"), maxBytes)
		if err != nil {
			return LoadedSkill{}, false, err
		}
		if asset != nil {
			return LoadedSkill{Asset: *asset, Directory: directory}, true, nil
		}
	}
	return LoadedSkill{}, false, nil
}
