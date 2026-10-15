package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"reflect"
	"slices"
	"sort"
	"strings"

	"jad/backend-go/internal/httpapi"
	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/transcript"
	"jad/backend-go/internal/webui"
)

type modelSummary struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// listingModelSummary names the model of a session for the session list. It reads
// the model file the session's settings select (config.ModelPath: the active
// profile's model_default, or model.json beside setting.json) without validating
// it, as the TypeScript list does: a missing or malformed file, or a field that
// is not a non-empty string, falls back to provider "unknown" and the session id
// as the model name, so one broken model file never hides a session.
func listingModelSummary(location sessionfs.Location, config sessionfs.SessionConfig) modelSummary {
	summary := modelSummary{Provider: "unknown", Model: location.SessionID}
	contents, err := os.ReadFile(config.ModelPath)
	if err != nil {
		return summary
	}
	var fields struct {
		Provider any `json:"provider"`
		Model    any `json:"model"`
	}
	if json.Unmarshal(contents, &fields) != nil {
		return summary
	}
	if provider, ok := fields.Provider.(string); ok && strings.TrimSpace(provider) != "" {
		summary.Provider = strings.TrimSpace(provider)
	}
	if model, ok := fields.Model.(string); ok && strings.TrimSpace(model) != "" {
		summary.Model = strings.TrimSpace(model)
	}
	return summary
}

type sessionListRecord struct {
	ID              string                             `json:"id"`
	SessionID       string                             `json:"session_id"`
	RootID          string                             `json:"root_id"`
	RootLabel       string                             `json:"root_label"`
	SessionName     string                             `json:"session_name"`
	Category        string                             `json:"category,omitempty"`
	Turns           int64                              `json:"turns"`
	ProfileTurns    map[string]int64                   `json:"profile_turns,omitempty"`
	ActiveProfile   string                             `json:"active_profile,omitempty"`
	DefaultProfile  string                             `json:"default_profile,omitempty"`
	Profiles        []string                           `json:"profiles,omitempty"`
	ProfileNames    map[string]string                  `json:"profile_names,omitempty"`
	ModelDefault    string                             `json:"model_default,omitempty"`
	Models          *[]string                          `json:"models,omitempty"`
	FrontCompaction json.RawMessage                    `json:"front_compaction,omitempty"`
	Usage           *webui.UsageSummary                `json:"usage,omitempty"`
	Run             *httpapi.SessionRunState           `json:"run,omitempty"`
	RunsByProfile   map[string]httpapi.SessionRunState `json:"runs_by_profile,omitempty"`
	Model           modelSummary                       `json:"model"`
}

type sessionPage struct {
	HasMore     bool   `json:"has_more"`
	NextBefore  *int64 `json:"next_before"`
	Generation  string `json:"generation"`
	StaleCursor bool   `json:"stale_cursor"`
}

type transcriptIndex struct {
	starts     []int64
	turns      int64
	size       int64
	generation string
	usage      webui.UsageSummary
	// lastResponseKey identifies the newest response already counted in usage,
	// so a rewritten usage checkpoint keeps deduplicating it.
	lastResponseKey string
}

// ListSessions implements httpapi.SessionCatalog. This bounded catalog uses
// session settings and the authoritative transcript; cache overlays and run
// summaries remain deferred.
func (r *Resolver) ListSessions(ctx context.Context, ifNoneMatch string, runStates httpapi.SessionRunProvider) (httpapi.CatalogResponse, error) {
	if ctx == nil {
		return httpapi.CatalogResponse{}, errors.New("session catalog context is required")
	}
	if err := ctx.Err(); err != nil {
		return httpapi.CatalogResponse{}, err
	}

	type candidate struct {
		location sessionfs.Location
		config   sessionfs.SessionConfig
		runs     httpapi.SessionRunSummaries
	}
	candidates := make([]candidate, 0)
	versions := make([]string, 0)
	// The sessions of one list share their catalogs: each directory is scanned once.
	scans := sessionfs.NewCatalogScans()
	for _, root := range r.roots {
		if err := ctx.Err(); err != nil {
			return httpapi.CatalogResponse{}, err
		}
		entries, err := os.ReadDir(root.Directory)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return httpapi.CatalogResponse{}, fmt.Errorf("read session root %q: %w", root.ID, err)
		}
		for _, entry := range entries {
			id, ok := sessionfs.SessionFileID(entry)
			if !ok {
				continue
			}
			location, err := sessionfs.Resolve(r.roots, root.ID+":"+id)
			if err != nil {
				var resolveErr *sessionfs.ResolveError
				if errors.As(err, &resolveErr) {
					continue
				}
				return httpapi.CatalogResponse{}, err
			}
			hasSetting, err := hasSessionSetting(location)
			if err != nil {
				return httpapi.CatalogResponse{}, err
			}
			if !hasSetting {
				continue
			}
			// The list needs only the settings: a session whose model.json is
			// missing or malformed is still listed, like in the TypeScript backend.
			config, err := sessionfs.LoadSessionSettingsScoped(location, "", "", scans)
			if err != nil {
				var configErr *sessionfs.ConfigError
				if errors.As(err, &configErr) {
					r.skipped.report(r.logger, location.Reference, configErr.Error())
					continue
				}
				return httpapi.CatalogResponse{}, mapSessionError(err)
			}
			r.skipped.resolved(location.Reference)
			sessionRuns := httpapi.SessionRunSummaries{}
			if runStates != nil {
				sessionRuns = runStates.SessionRuns(location.Reference, config.Profile)
			}
			candidates = append(candidates, candidate{location: location, config: config, runs: sessionRuns})
			versions = append(versions, location.Reference+":"+sessionVersionForCatalog(location, config, r.catalog.MaxInlinePartBytes, sessionRuns)+profileTranscriptStamps(location, config))
		}
	}
	sort.Strings(versions)
	etag := makeETag(strings.Join(versions, "|"))
	if httpapi.ETagMatches(ifNoneMatch, etag) {
		return httpapi.CatalogResponse{ETag: etag, NotModified: true}, nil
	}

	sessions := make([]sessionListRecord, 0, len(candidates))
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return httpapi.CatalogResponse{}, err
		}
		shared, err := r.sharedStore(candidate.config)
		if err != nil {
			return httpapi.CatalogResponse{}, err
		}
		index, err := scanTranscriptIndex(shared, candidate.config)
		if err != nil {
			return httpapi.CatalogResponse{}, fmt.Errorf("read session transcript %q: %w", candidate.location.Reference, err)
		}
		record := makeSessionListRecord(candidate.location, candidate.config, index.turns, index.usage, candidate.runs)
		record.ProfileTurns = r.profileTurns(candidate.location, candidate.config, index.turns)
		record.Model = listingModelSummary(candidate.location, candidate.config)
		// The TypeScript list reports usage only from an existing usage cache, so
		// a session that has never completed a response carries no usage block.
		if usageIsEmpty(index.usage) {
			record.Usage = nil
		}
		sessions = append(sessions, record)
	}
	finalVersions := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		finalVersions = append(finalVersions, candidate.location.Reference+":"+sessionVersionForCatalog(candidate.location, candidate.config, r.catalog.MaxInlinePartBytes, candidate.runs)+profileTranscriptStamps(candidate.location, candidate.config))
	}
	sort.Strings(finalVersions)
	etag = makeETag(strings.Join(finalVersions, "|"))
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].ID < sessions[j].ID })
	body, err := json.Marshal(struct {
		Sessions []sessionListRecord `json:"sessions"`
	}{Sessions: sessions})
	if err != nil {
		return httpapi.CatalogResponse{}, fmt.Errorf("encode session list: %w", err)
	}
	return httpapi.CatalogResponse{Body: body, ETag: etag}, nil
}

// profileTranscriptStamps changes when any of the other profiles' transcripts does,
// so a list validator covers the counts it reports. It names the transcripts by path
// and loads no profile, which keeps a 304 cheap. It is empty, and the validator
// unchanged, for a session with no other profile.
func profileTranscriptStamps(location sessionfs.Location, config sessionfs.SessionConfig) string {
	parts := make([]string, 0, 2*len(config.Profiles))
	for _, profile := range config.Profiles {
		if profile == config.Profile {
			continue
		}
		if path, ok := location.ProfileMessagesPath(profile); ok {
			parts = append(parts, profile, fileStamp(path))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return ":" + strings.Join(parts, "\x00")
}

// profileTurns is the turn count of each profile in scope. A profile that has never
// run has no transcript file, and counts 0 without creating one or loading the profile.
// A profile whose transcript cannot be read or loaded is left out, so one damaged
// profile does not fail the list. A count is reused while the transcript's file stamp
// is unchanged, so an idle profile costs one stat per list.
func (r *Resolver) profileTurns(location sessionfs.Location, config sessionfs.SessionConfig, turns int64) map[string]int64 {
	if len(config.Profiles) == 0 {
		return nil
	}
	counts := map[string]int64{config.Profile: turns}
	for _, profile := range config.Profiles {
		if profile == config.Profile {
			continue
		}
		path, ok := location.ProfileMessagesPath(profile)
		if !ok {
			continue
		}
		if _, err := os.Stat(path); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				counts[profile] = 0
			}
			continue
		}
		stamp := fileStamp(path)
		if count, ok := r.cachedProfileTurns(path, stamp); ok {
			counts[profile] = count
			continue
		}
		other, err := sessionfs.LoadSessionSettings(location, profile, "")
		if err != nil {
			continue
		}
		shared, err := r.sharedStore(other)
		if err != nil {
			continue
		}
		index, err := scanTranscriptIndex(shared, other)
		if err != nil {
			continue
		}
		counts[profile] = index.turns
		// The stamp was taken before the scan, so a transcript that grew meanwhile
		// has a different stamp on the next list and is counted again.
		r.rememberProfileTurns(path, stamp, index.turns)
	}
	return counts
}

func (r *Resolver) cachedProfileTurns(path, stamp string) (int64, bool) {
	r.profileTurnsMu.Lock()
	defer r.profileTurnsMu.Unlock()
	entry, ok := r.profileTurnCounts[path]
	return entry.turns, ok && entry.stamp == stamp
}

func (r *Resolver) rememberProfileTurns(path, stamp string, turns int64) {
	r.profileTurnsMu.Lock()
	defer r.profileTurnsMu.Unlock()
	r.profileTurnCounts[path] = profileTurnsEntry{stamp: stamp, turns: turns}
}

// ReadSessionSnapshot implements the transcript-backed portion of the
// TypeScript snapshot contract: profile metadata, messages, turns, page cursors,
// and a stable validator. Cache-derived history/usage and message-part fetch
// cursors are not yet ported.
func (r *Resolver) ReadSessionSnapshot(ctx context.Context, reference, profile string, options httpapi.SnapshotOptions, ifNoneMatch string, runStates httpapi.SessionRunProvider) (httpapi.CatalogResponse, error) {
	if ctx == nil {
		return httpapi.CatalogResponse{}, errors.New("session snapshot context is required")
	}
	if err := ctx.Err(); err != nil {
		return httpapi.CatalogResponse{}, err
	}
	location, err := sessionfs.Resolve(r.roots, reference)
	if err != nil {
		return httpapi.CatalogResponse{}, mapSessionError(err)
	}
	config, err := sessionfs.LoadSessionConfig(location, profile, "")
	if err != nil {
		return httpapi.CatalogResponse{}, mapSessionError(err)
	}
	runs := httpapi.SessionRunSummaries{}
	if runStates != nil {
		runs = runStates.SessionRuns(location.Reference, config.Profile)
	}
	etag := sessionVersionForCatalog(location, config, r.catalog.MaxInlinePartBytes, runs)
	if httpapi.ETagMatches(ifNoneMatch, etag) {
		return httpapi.CatalogResponse{ETag: etag, NotModified: true}, nil
	}
	shared, err := r.sharedStore(config)
	if err != nil {
		return httpapi.CatalogResponse{}, err
	}
	index, err := scanTranscriptIndex(shared, config)
	if err != nil {
		return httpapi.CatalogResponse{}, fmt.Errorf("read session transcript %q: %w", location.Reference, err)
	}
	messages, page, err := r.readMessagePage(ctx, shared.store, index, options)
	if err != nil {
		return httpapi.CatalogResponse{}, err
	}
	// A cursor page asks for older tail turns only; the caller already holds the
	// history block, and a page does not restate it.
	var historyRef *historyReference
	if options.Before == nil {
		history, err := r.readHistoryBlock(ctx, config)
		if err != nil {
			return httpapi.CatalogResponse{}, err
		}
		historyRef = &historyReference{Count: len(history.Messages)}
		if history.ETag != "" {
			etagValue := history.ETag
			historyRef.ETag = &etagValue
		}
	}
	etag = sessionVersionForCatalog(location, config, r.catalog.MaxInlinePartBytes, runs)
	item := makeSessionListRecord(location, config, index.turns, index.usage, runs)
	body, err := makeSnapshotBody(item, messages, page, historyRef, shared.store.ReadRunErrors())
	if err != nil {
		return httpapi.CatalogResponse{}, fmt.Errorf("encode session snapshot: %w", err)
	}
	return httpapi.CatalogResponse{Body: body, ETag: etag}, nil
}

func (r *Resolver) readMessagePage(ctx context.Context, store *transcript.TurnStore, index transcriptIndex, options httpapi.SnapshotOptions) ([]json.RawMessage, sessionPage, error) {
	page := sessionPage{Generation: index.generation}
	messages := make([]json.RawMessage, 0)
	beforeIndex := len(index.starts)
	if options.Before != nil {
		beforeIndex = -1
		for index, start := range index.starts {
			if start == *options.Before {
				beforeIndex = index
				break
			}
		}
		if (options.CursorGeneration != "" && options.CursorGeneration != page.Generation) || beforeIndex < 0 {
			page.StaleCursor = true
			return messages, page, nil
		}
	}
	if beforeIndex <= 0 {
		return messages, page, nil
	}
	requestedTurns := r.catalog.SnapshotTurns
	if options.Turns != nil {
		requestedTurns = *options.Turns
	}
	limit := requestedTurns
	if limit > 0 && limit > r.catalog.MaxPageTurns {
		limit = r.catalog.MaxPageTurns
	}
	first := 0
	if limit > 0 && limit < beforeIndex {
		first = beforeIndex - limit
	}
	stop := index.size
	if beforeIndex < len(index.starts) {
		stop = index.starts[beforeIndex]
	}
	start := index.starts[first]
	if stop < start {
		return nil, sessionPage{}, errors.New("session page cursor range is invalid")
	}
	page.HasMore = first > 0
	if page.HasMore {
		next := index.starts[first]
		page.NextBefore = &next
	}
	records := make([]webui.TranscriptRecord, 0)
	err := store.ReadRecordsRange(start, stop, func(offset int64, record []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		cursor := cursorForOffset(index.starts, offset)
		records = append(records, webui.TranscriptRecord{
			Offset:    offset,
			Prefix:    "o",
			Raw:       append(json.RawMessage(nil), record...),
			Cursor:    cursor,
			HasCursor: true,
		})
		return nil
	})
	if err != nil {
		return nil, sessionPage{}, err
	}
	normalized, err := webui.NormalizeTranscript(records)
	if err != nil {
		return nil, sessionPage{}, err
	}
	normalized = webui.CapMessages(normalized, r.catalog.MaxInlinePartBytes, nil)
	messages = make([]json.RawMessage, 0, len(normalized))
	for _, message := range normalized {
		encoded, err := json.Marshal(message)
		if err != nil {
			return nil, sessionPage{}, err
		}
		messages = append(messages, encoded)
	}
	return messages, page, nil
}

func cursorForOffset(starts []int64, offset int64) int64 {
	turn := sort.Search(len(starts), func(index int) bool { return starts[index] > offset })
	if turn < 1 || len(starts) == 0 {
		return 0
	}
	return starts[turn-1]
}

func scanTranscriptIndex(shared *sharedSession, config sessionfs.SessionConfig) (transcriptIndex, error) {
	if shared == nil || shared.store == nil {
		return transcriptIndex{}, errors.New("shared transcript store is not initialized")
	}
	store := shared.store
	info, err := os.Stat(config.MessagesPath)
	generation := "0:0:0"
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return transcriptIndex{}, fmt.Errorf("stat transcript: %w", err)
	}
	if err == nil {
		generation = transcriptGeneration(config.MessagesPath, info)
	}
	starts, err := shared.readTurnStarts(generation)
	if err != nil {
		return transcriptIndex{}, fmt.Errorf("build transcript turn index: %w", err)
	}
	index := transcriptIndex{starts: starts}
	usageCache, cacheValid := readUsageCacheState(config)
	usageOffset := int64(0)
	lastResponseKey := ""
	var turnCount, maxTurn int64
	if cacheValid {
		index.usage = usageCache.Summary
		usageOffset = usageCache.Transcript.Offset
		lastResponseKey = usageCache.LastResponseKey
		turnCount = index.usage.TurnCount
		maxTurn = turnCount
	}
	seenBatches := make(map[string]struct{})
	seenResponses := make(map[string]struct{})
	seenUsageKeys := make(map[string]struct{})
	scanThrough := usageOffset
	err = store.ReadRecordsRange(usageOffset, 1<<63-1, func(offset int64, line []byte) error {
		scanThrough = offset + int64(len(line)) + 1
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(line, &fields); err != nil || fields == nil {
			return fmt.Errorf("transcript record at byte %d is not a JSON object", offset)
		}
		var role string
		_ = json.Unmarshal(fields["role"], &role)
		if role == "user" {
			var batchID string
			if raw, ok := fields[transcript.SessionInputBatchIDField]; ok && json.Unmarshal(raw, &batchID) == nil && batchID != "" {
				if _, seen := seenBatches[batchID]; !seen {
					seenBatches[batchID] = struct{}{}
					turnCount++
				}
			} else {
				turnCount++
			}
		}
		var recordType string
		_ = json.Unmarshal(fields["type"], &recordType)
		if recordType != "response.completed" {
			return nil
		}
		var response map[string]json.RawMessage
		if json.Unmarshal(fields["response"], &response) != nil {
			response = nil
		}
		responseKey := usageCacheResponseKey(line, response)
		if responseKey != "" {
			if responseKey == lastResponseKey {
				return nil
			}
			if _, seen := seenUsageKeys[responseKey]; seen {
				return nil
			}
			seenUsageKeys[responseKey] = struct{}{}
			lastResponseKey = responseKey
		}
		var sessionTurn int64
		hasTurn := false
		if raw, ok := fields["session_turn"]; ok {
			if err := json.Unmarshal(raw, &sessionTurn); err != nil || sessionTurn < 1 {
				return fmt.Errorf("session_turn at byte %d must be a positive integer", offset)
			}
			hasTurn = true
			if sessionTurn > maxTurn {
				maxTurn = sessionTurn
			}
		}
		if !hasTurn && turnCount == 0 {
			turnCount = 1
		}
		if turnCount > index.usage.TurnCount {
			index.usage.TurnCount = turnCount
		}
		if err := webui.AddCompletedRecord(&index.usage, line, fields, response, seenResponses); err != nil {
			return fmt.Errorf("accumulate usage at byte %d: %w", offset, err)
		}
		return nil
	})
	if err != nil {
		return transcriptIndex{}, err
	}
	if maxTurn > turnCount {
		turnCount = maxTurn
	}
	index.turns = turnCount
	index.lastResponseKey = lastResponseKey
	if turnCount > index.usage.TurnCount {
		index.usage.TurnCount = turnCount
	}
	if !cacheValid {
		if backupInfo, backupErr := os.Stat(config.MessagesBackupPath); backupErr == nil && backupInfo.Size() > 0 {
			index.usage.HistoryPartial = true
		}
	}
	info, err = os.Stat(config.MessagesPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return index, nil
		}
		return transcriptIndex{}, fmt.Errorf("stat transcript: %w", err)
	}
	index.size = info.Size()
	index.generation = transcriptGeneration(config.MessagesPath, info)
	if (!cacheValid || scanThrough > usageOffset) && scanThrough <= index.size {
		if err := writeUsageCacheCheckpoint(config, index.usage, scanThrough, lastResponseKey); err != nil {
			return transcriptIndex{}, fmt.Errorf("write session usage cache: %w", err)
		}
	}
	return index, nil
}

func makeSessionListRecord(location sessionfs.Location, config sessionfs.SessionConfig, turns int64, usage webui.UsageSummary, runs httpapi.SessionRunSummaries) sessionListRecord {
	item := sessionListRecord{
		ID:              location.Reference,
		SessionID:       location.SessionID,
		RootID:          location.Root.ID,
		RootLabel:       location.Root.Label,
		SessionName:     config.SessionName,
		Category:        config.Category,
		Turns:           turns,
		ActiveProfile:   config.Profile,
		DefaultProfile:  config.DefaultProfile,
		Profiles:        append([]string(nil), config.Profiles...),
		ProfileNames:    maps.Clone(config.ProfileNames),
		ModelDefault:    config.ModelDefault,
		FrontCompaction: append(json.RawMessage(nil), config.FrontCompaction...),
		Model: modelSummary{
			Provider: config.Model.Provider,
			Model:    config.Model.Name,
		},
	}
	if config.Models != nil {
		models := append([]string{}, config.Models...)
		item.Models = &models
	}
	usageCopy := usage
	item.Usage = &usageCopy
	if runs.Default != nil {
		defaultRun := *runs.Default
		item.Run = &defaultRun
	}
	if len(runs.ByProfile) > 0 {
		item.RunsByProfile = make(map[string]httpapi.SessionRunState, len(runs.ByProfile))
		for profile, run := range runs.ByProfile {
			item.RunsByProfile[profile] = run
		}
	}
	return item
}

func makeSnapshotBody(item sessionListRecord, messages []json.RawMessage, page sessionPage, history *historyReference, failures []transcript.RunErrorEntry) (json.RawMessage, error) {
	base, err := json.Marshal(item)
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(base, &fields); err != nil {
		return nil, err
	}
	fields["messages"], err = json.Marshal(messages)
	if err != nil {
		return nil, err
	}
	fields["page"], err = json.Marshal(page)
	if err != nil {
		return nil, err
	}
	if history != nil {
		fields["history"], err = json.Marshal(history)
		if err != nil {
			return nil, err
		}
	}
	// Omitted when there is none, so a session that never failed keeps its payload.
	if len(failures) > 0 {
		fields["failures"], err = json.Marshal(failures)
		if err != nil {
			return nil, err
		}
	}
	return json.Marshal(fields)
}

func hasSessionSetting(location sessionfs.Location) (bool, error) {
	return location.HasSetting()
}

func sessionVersionForCatalog(location sessionfs.Location, config sessionfs.SessionConfig, maxInlinePartBytes int, runs httpapi.SessionRunSummaries) string {
	runState, _ := json.Marshal(runs)
	return makeETag(strings.Join([]string{
		sessionVersionForLocation(location, config),
		fmt.Sprint(maxInlinePartBytes),
		string(runState),
	}, "|"))
}

func sessionVersionForLocation(location sessionfs.Location, config sessionfs.SessionConfig) string {
	parts := []string{
		location.Reference,
		location.Root.ID,
		location.Root.Label,
		sessionVersion(config),
	}
	return makeETag(strings.Join(parts, "|"))
}

func sessionVersion(config sessionfs.SessionConfig) string {
	parts := []string{
		config.Profile,
		config.DefaultProfile,
		config.Category,
		config.ModelDefault,
		strings.Join(config.Profiles, "\x00"),
		profileNamesVersion(config.ProfileNames),
		fileStamp(config.MessagesPath),
		fileStamp(config.MessagesBackupPath),
		fileStamp(config.UsageCachePath),
		// A failed run changes the payload without touching the transcript.
		fileStamp(config.StoragePaths.RunErrorCache),
		fmt.Sprint(config.BackupTurns),
		fileStamp(config.SettingPath),
		fileStamp(config.ModelPath),
	}
	return makeETag(strings.Join(parts, "|"))
}

// profileNamesVersion is the names in profile ID order, so renaming a profile changes the version.
func profileNamesVersion(names map[string]string) string {
	ids := slices.Sorted(maps.Keys(names))
	parts := make([]string, 0, 2*len(ids))
	for _, id := range ids {
		parts = append(parts, id, names[id])
	}
	return strings.Join(parts, "\x00")
}

func fileStamp(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return "-"
	}
	return fmt.Sprintf("%s:%d:%d:%s", transcriptGeneration(path, info), info.Size(), info.ModTime().UnixNano(), statChangeTime(info))
}

func statChangeTime(info os.FileInfo) string {
	stat := reflect.ValueOf(info.Sys())
	if stat.Kind() == reflect.Pointer {
		if stat.IsNil() {
			return "-"
		}
		stat = stat.Elem()
	}
	if stat.Kind() != reflect.Struct {
		return "-"
	}
	for _, name := range []string{"Ctim", "Ctimespec"} {
		field := stat.FieldByName(name)
		if field.IsValid() && field.CanInterface() {
			return fmt.Sprintf("%#v", field.Interface())
		}
	}
	return "-"
}

// transcriptGeneration identifies one incarnation of a file as
// "device:inode:birth-ms", like the TypeScript store: a replaced file changes it
// even when the filesystem reuses its inode. path supplies the birth time,
// which os.FileInfo does not expose.
func transcriptGeneration(path string, info os.FileInfo) string {
	if info == nil {
		return "0:0:0"
	}
	stat := reflect.ValueOf(info.Sys())
	if stat.Kind() == reflect.Pointer && !stat.IsNil() {
		stat = stat.Elem()
	}
	dev, okDev := unsignedStatField(stat, "Dev")
	ino, okIno := unsignedStatField(stat, "Ino")
	if !okDev || !okIno {
		return "0:0:0"
	}
	return fmt.Sprintf("%d:%d:%d", dev, ino, fileBirthMillis(path))
}

func unsignedStatField(stat reflect.Value, name string) (uint64, bool) {
	if stat.Kind() != reflect.Struct {
		return 0, false
	}
	field := stat.FieldByName(name)
	if !field.IsValid() {
		return 0, false
	}
	switch field.Kind() {
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return field.Uint(), true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		value := field.Int()
		if value >= 0 {
			return uint64(value), true
		}
	}
	return 0, false
}

func makeETag(value string) string {
	hash := sha256.Sum256([]byte(value))
	return `"` + hex.EncodeToString(hash[:]) + `"`
}

// usageIsEmpty reports a summary with nothing to show: no completed responses,
// no turns, and no in-flight response.
func usageIsEmpty(usage webui.UsageSummary) bool {
	return usage.TurnCount == 0 && usage.CompletedResponses == 0 && !usage.HistoryPartial && usage.Current == nil
}
