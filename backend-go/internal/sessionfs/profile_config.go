package sessionfs

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// SettingSources are the setting layers of a session's profile, in merge order, and the
// settings they composed to (the layoutSettingKeys of the composed setting).
type SettingSources struct {
	Layers    []SettingLayer
	Effective map[string]json.RawMessage
}

// settingSources keeps the layers compose merged and the settings keys of the result. The
// layers are the layout's default layers (built-in, global, workspace) or its one root
// defaults layer in v1, then the session's and the profile's settings.
func (c *layoutCompose) settingSources(profileSettings, composed map[string]json.RawMessage) *SettingSources {
	layers := slices.Clone(c.layout.DefaultLayers)
	if len(layers) == 0 {
		layers = []SettingLayer{{Name: SettingLayerDefaults, Values: c.layout.Defaults}}
	}
	layers = append(layers, SettingLayer{Name: SettingLayerSession, Values: c.sessionLayer}, SettingLayer{Name: SettingLayerProfile, Values: profileSettings})
	effective := make(map[string]json.RawMessage, len(layoutSettingKeys))
	for key, value := range composed {
		if _, setting := layoutSettingKeys[key]; setting {
			effective[key] = value
		}
	}
	return &SettingSources{Layers: layers, Effective: effective}
}

// sourceNode is one value of the layer merge with the layers that set it.
type sourceNode struct {
	layers   []string
	children map[string]*sourceNode
}

// mergeSource mirrors mergeSettingValue: an object over an object merges key by key, and
// anything else replaces, so the replaced value's layers are dropped with it.
func mergeSource(base *sourceNode, raw json.RawMessage, layer string) *sourceNode {
	var object map[string]json.RawMessage
	isObject := json.Unmarshal(raw, &object) == nil && object != nil
	if !isObject {
		return &sourceNode{layers: []string{layer}}
	}
	if base == nil || base.children == nil {
		base = &sourceNode{children: map[string]*sourceNode{}}
	}
	if !slices.Contains(base.layers, layer) {
		base.layers = append(base.layers, layer)
	}
	for key, value := range object {
		base.children[key] = mergeSource(base.children[key], value, layer)
	}
	return base
}

// flatten lists the leaves of node under prefix: a value that is not an object, and an
// empty object, is one leaf; an array is one value.
func (n *sourceNode) flatten(prefix string, into map[string][]string) {
	if len(n.children) == 0 {
		into[prefix] = n.layers
		return
	}
	for key, child := range n.children {
		child.flatten(prefix+"."+key, into)
	}
}

// SetBy maps each effective setting, by its dotted path down to the leaves of objects (an
// array is one value), to the layers that set it, in merge order. A key whose value is not
// what the layers merge to (sandbox, whose layers compose with templates and added lists, or
// hooks a toolset adds) is one entry naming every layer that has the key, or the toolset
// when none has it.
func (s SettingSources) SetBy() map[string][]string {
	setBy := map[string][]string{}
	for _, key := range slices.Sorted(maps.Keys(s.Effective)) {
		var node *sourceNode
		var having []string
		merged := json.RawMessage(nil)
		for _, layer := range s.Layers {
			value, exists := layer.Values[key]
			if !exists {
				continue
			}
			having = append(having, layer.Name)
			node = mergeSource(node, value, layer.Name)
			merged = mergeSettingValue(merged, value)
		}
		if node == nil || !sameJSON(merged, s.Effective[key]) {
			if len(having) == 0 {
				having = []string{SettingLayerToolset}
			}
			setBy[key] = having
			continue
		}
		node.flatten(key, setBy)
	}
	return setBy
}

// sameJSON reports whether two JSON values are equal as values.
func sameJSON(a, b json.RawMessage) bool {
	var left, right any
	if json.Unmarshal(a, &left) != nil || json.Unmarshal(b, &right) != nil {
		return bytes.Equal(bytes.TrimSpace(a), bytes.TrimSpace(b))
	}
	encodedLeft, errLeft := json.Marshal(left)
	encodedRight, errRight := json.Marshal(right)
	return errLeft == nil && errRight == nil && bytes.Equal(encodedLeft, encodedRight)
}

// ProfileConfig is what the profile tool's config action shows a profile of itself.
type ProfileConfig struct {
	Profile string `json:"profile"`
	// File is the profile file, Revision its entity tag (CatalogETag) and Contents its JSON.
	File     string          `json:"file"`
	Revision string          `json:"revision"`
	Contents json.RawMessage `json:"contents"`
	// Effective are the settings the profile runs with; SetBy says which layer set each one.
	// A setting that is in neither takes its built-in default.
	Effective map[string]json.RawMessage `json:"effective"`
	SetBy     map[string][]string        `json:"set_by"`
	Layers    []string                   `json:"layers"`
	// OwnDir and DataDir are the profile's private folder and working data folder (workspace
	// layout only).
	OwnDir  string `json:"own_dir,omitempty"`
	DataDir string `json:"data_dir,omitempty"`
}

// ReadProfileConfig reads the profile file of a loaded catalog-layout configuration and
// describes its settings. The revision is that of the bytes shown.
func ReadProfileConfig(config SessionConfig) (ProfileConfig, error) {
	if config.ProfilePath == "" || config.SettingSources == nil {
		return ProfileConfig{}, errors.New("the profile's configuration is known only in a catalog-layout session")
	}
	contents, err := readRegularFile(config.ProfilePath, maxInternalJSONFileBytes)
	if err != nil {
		return ProfileConfig{}, fmt.Errorf("read profile file %s: %w", config.ProfilePath, err)
	}
	if !json.Valid(contents) {
		return ProfileConfig{}, fmt.Errorf("profile file %s is not valid JSON", config.ProfilePath)
	}
	layers := make([]string, 0, len(config.SettingSources.Layers))
	for _, layer := range config.SettingSources.Layers {
		layers = append(layers, layer.Name)
	}
	return ProfileConfig{
		Profile:   config.Profile,
		File:      config.ProfilePath,
		Revision:  CatalogETag(contents),
		Contents:  json.RawMessage(strings.TrimSpace(string(contents))),
		Effective: maps.Clone(config.SettingSources.Effective),
		SetBy:     config.SettingSources.SetBy(),
		Layers:    layers,
		OwnDir:    config.ProfileOwnDir,
		DataDir:   config.ProfileDataDir,
	}, nil
}
