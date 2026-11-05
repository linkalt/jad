package app

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"jad/backend-go/internal/asyncmgr"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools/native"
	"jad/backend-go/internal/transcript"
)

type stateContextCollector func() (any, error)

type stateContextProviderValue struct {
	name  string
	value any
}

type orderedStateContextProviders []stateContextProviderValue

func (providers orderedStateContextProviders) MarshalJSON() ([]byte, error) {
	var output bytes.Buffer
	output.WriteByte('{')
	for index, provider := range providers {
		if index > 0 {
			output.WriteByte(',')
		}
		name, err := json.Marshal(provider.name)
		if err != nil {
			return nil, err
		}
		value, err := marshalStateContextValue(provider.value)
		if err != nil {
			return nil, err
		}
		output.Write(name)
		output.WriteByte(':')
		output.Write(value)
	}
	output.WriteByte('}')
	return output.Bytes(), nil
}

func marshalStateContextValue(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte{10}), nil
}

type stateContextDocument struct {
	Trigger     string                       `json:"trigger"`
	GeneratedAt string                       `json:"generated_at"`
	Providers   orderedStateContextProviders `json:"providers"`
}

type stateContextTrimmer interface {
	StateContextItemCount() int
	TrimStateContextItem() bool
}

type stateContextQueryTrimmer interface {
	StateContextQueryCount() int
	TrimStateContextQuery() bool
}

// stateContextSources are the live stores the providers read. A source a session does
// not configure may be nil.
type stateContextSources struct {
	task   native.TaskStateSource
	memory *native.MemoryStore
	// subagent holds the child runs; owner is the session key whose runs are listed.
	subagent *native.SubagentManager
	owner    string
	// async is the manager of the session's async handles; nil without the async tool.
	async *asyncmgr.Manager
}

func collectStateContextSnapshot(
	config sessionfs.SessionConfig,
	sources stateContextSources,
	trigger string,
) (transcript.StateContextSnapshot, bool, error) {
	return collectStateContextSnapshotWithPrompt(config, sources, trigger, "")
}

// collectStateContextSnapshotWithPrompt adds a prompt typed by the user after the
// configured one. A typed prompt is itself content, so the user trigger writes a block
// with it even when the stores are empty.
func collectStateContextSnapshotWithPrompt(
	config sessionfs.SessionConfig,
	sources stateContextSources,
	trigger string,
	typedPrompt string,
) (transcript.StateContextSnapshot, bool, error) {
	snapshot, hasData, err := buildStateContextSnapshot(config, sources, trigger, typedPrompt)
	if err != nil || (!hasData && typedPrompt == "") {
		return transcript.StateContextSnapshot{}, false, err
	}
	return snapshot, true, nil
}

// collectRunStartStateContext decides whether the run_start trigger writes a block: only
// when the data differs from the latest stored block. A state that became empty differs
// from a block that reported data, so one block says so; an empty state after an empty
// block, or with no block at all, writes nothing.
func collectRunStartStateContext(
	config sessionfs.SessionConfig,
	sources stateContextSources,
	latest *transcript.StateContextEntry,
) (transcript.StateContextSnapshot, bool, error) {
	if !config.StateContext.Enabled {
		return transcript.StateContextSnapshot{}, false, nil
	}
	if _, configured := config.StateContext.Triggers[sessionfs.StateContextTriggerRunStart]; !configured {
		return transcript.StateContextSnapshot{}, false, nil
	}
	snapshot, hasData, err := buildStateContextSnapshot(config, sources, sessionfs.StateContextTriggerRunStart, "")
	if err != nil {
		return transcript.StateContextSnapshot{}, false, err
	}
	if latest == nil && !hasData {
		return transcript.StateContextSnapshot{}, false, nil
	}
	if latest != nil && latest.DataSHA256 == snapshot.DataSHA256 {
		return transcript.StateContextSnapshot{}, false, nil
	}
	return snapshot, true, nil
}

// buildStateContextSnapshot always renders the block, even when no provider returned an
// item, and reports whether any did; the caller decides whether an empty one is written.
// An unconfigured trigger gives a zero snapshot.
func buildStateContextSnapshot(
	config sessionfs.SessionConfig,
	sources stateContextSources,
	trigger string,
	typedPrompt string,
) (transcript.StateContextSnapshot, bool, error) {
	settings := config.StateContext
	triggerSettings, configured := settings.Triggers[trigger]
	if !settings.Enabled || !configured {
		return transcript.StateContextSnapshot{}, false, nil
	}
	collectors := map[string]stateContextCollector{}
	if config.NativeTask != nil {
		collectors["task"] = func() (any, error) {
			return native.CollectTaskStateContext(sources.task, config.NativeTask.StateContext)
		}
	}
	if config.NativeMemory != nil {
		collectors["memory"] = func() (any, error) {
			return native.CollectMemoryStateContext(sources.memory, *config.NativeMemory, config.NativeMemory.StateContext)
		}
	}
	if config.NativeSubagent != nil {
		collectors["subagent"] = func() (any, error) {
			return native.CollectSubagentStateContext(sources.subagent, sources.owner, config.NativeSubagent.StateContext)
		}
	}
	if config.NativeAsync != nil {
		collectors["async"] = func() (any, error) {
			return native.CollectAsyncStateContext(sources.async, sources.owner)
		}
	}
	providers := make(orderedStateContextProviders, 0, len(triggerSettings.Tools))
	hasData := false
	for _, name := range triggerSettings.Tools {
		collect, exists := collectors[name]
		if !exists {
			return transcript.StateContextSnapshot{}, false, fmt.Errorf("statecontext provider %s is not configured under tools.native", name)
		}
		value, err := collect()
		if err != nil {
			return transcript.StateContextSnapshot{}, false, err
		}
		providers = append(providers, stateContextProviderValue{name: name, value: value})
		hasData = hasData || stateContextProviderHasData(value)
	}
	generatedAt := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	document := stateContextDocument{Trigger: trigger, GeneratedAt: generatedAt, Providers: providers}
	lead := settings.Header
	if triggerSettings.Prompt != "" {
		lead += "\n" + triggerSettings.Prompt
	}
	if typedPrompt != "" {
		lead += "\n" + typedPrompt
	}
	rendered, err := renderStateContextDocument(lead, document)
	if err != nil {
		return transcript.StateContextSnapshot{}, false, err
	}
	for int64(len(rendered)) > triggerSettings.MaxBytes {
		var selected stateContextTrimmer
		selectedCount := 0
		for _, provider := range providers {
			trimmer, ok := provider.value.(stateContextTrimmer)
			if !ok {
				continue
			}
			count := trimmer.StateContextItemCount()
			if count > selectedCount {
				selected = trimmer
				selectedCount = count
			}
		}
		if selected == nil || !selected.TrimStateContextItem() {
			var querySelected stateContextQueryTrimmer
			for _, provider := range providers {
				trimmer, ok := provider.value.(stateContextQueryTrimmer)
				if ok && trimmer.StateContextQueryCount() > 0 {
					querySelected = trimmer
					break
				}
			}
			if querySelected == nil || !querySelected.TrimStateContextQuery() {
				return transcript.StateContextSnapshot{}, false, fmt.Errorf("statecontext metadata exceeds statecontext.triggers.%s.max_bytes", trigger)
			}
		}
		rendered, err = renderStateContextDocument(lead, document)
		if err != nil {
			return transcript.StateContextSnapshot{}, false, err
		}
	}
	dataSHA256, err := stateContextDataSHA256(providers, hasData)
	if err != nil {
		return transcript.StateContextSnapshot{}, false, err
	}
	return transcript.StateContextSnapshot{
		SchemaVersion: 1,
		GeneratedAt:   generatedAt,
		Trigger:       trigger,
		Text:          rendered,
		DataSHA256:    dataSHA256,
	}, hasData, nil
}

// stateContextDataSHA256 fingerprints what the model would be told, not when: the provider
// data after trimming. Every empty state shares one fingerprint.
func stateContextDataSHA256(providers orderedStateContextProviders, hasData bool) (string, error) {
	content := []byte("empty")
	if hasData {
		encoded, err := providers.MarshalJSON()
		if err != nil {
			return "", fmt.Errorf("encode statecontext data: %w", err)
		}
		content = encoded
	}
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:]), nil
}

// stateContextProviderHasData reports whether a provider returned anything the model
// would act on; a provider that cannot count its items is assumed to have data.
func stateContextProviderHasData(value any) bool {
	counter, ok := value.(stateContextTrimmer)
	return !ok || counter.StateContextItemCount() > 0
}

func renderStateContextDocument(header string, document stateContextDocument) (string, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(document); err != nil {
		return "", fmt.Errorf("encode statecontext snapshot: %w", err)
	}
	encoded := bytes.TrimSuffix(buffer.Bytes(), []byte{'\n'})
	return header + "\n" + string(encoded), nil
}
