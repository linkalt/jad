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
	if config.ModelPath == "" {
		return summary
	}
	// A profile's own model is read composed with its base (sessionfs.ModelBase).
	fields, err := config.ModelFields(config.ModelPath)
	if err != nil {
		return summary
	}
	var provider, model string
	if json.Unmarshal(fields["provider"], &provider) == nil && strings.TrimSpace(provider) != "" {
		summary.Provider = strings.TrimSpace(provider)
	}
	if json.Unmarshal(fields["model"], &model) == nil && strings.TrimSpace(model) != "" {
		summary.Model = strings.TrimSpace(model)
	}
	return summary
}

type modelOption = httpapi.SessionModelOption

// listingModelOptions describes every model the profile offers, in order, reading
// each file raw like listingModelSummary: a file that cannot be read gives provider
// "unknown" and no efforts, and never fails or hides a session. The loader has already
// left out every model whose file fails the model rules, the profile's model_default
// included, so the raw read only meets a file that changed after the load.
func listingModelOptions(config sessionfs.SessionConfig) []modelOption {
	if config.Models == nil {
		return nil
	}
	options := make([]modelOption, 0, len(config.Models))
	for _, id := range config.Models {
		option := modelOption{ID: id, Provider: "unknown"}
		if config.OwnModel(config.ModelPaths[id]) {
			option.Scope = sessionfs.OwnModelScope
		}
		fields, err := config.ModelFields(config.ModelPaths[id])
		if err == nil {
			var provider string
			if json.Unmarshal(fields["provider"], &provider) == nil && strings.TrimSpace(provider) != "" {
				option.Provider = strings.TrimSpace(provider)
			}
			var list []json.RawMessage
			if json.Unmarshal(fields["efforts"], &list) == nil && list != nil {
				efforts := make([]string, 0, len(list))
				for _, item := range list {
					var effort string
					if json.Unmarshal(item, &effort) != nil {
						efforts = nil
						break
					}
					efforts = append(efforts, effort)
				}
				option.Efforts = efforts
			}
			var request, reasoning map[string]json.RawMessage
			if json.Unmarshal(fields["request"], &request) == nil &&
				json.Unmarshal(request["reasoning"], &reasoning) == nil {
				var effort string
				if json.Unmarshal(reasoning["effort"], &effort) == nil {
					option.DefaultEffort = effort
				}
			}
		}
		options = append(options, option)
	}
	return options
}

type sessionListRecord struct {
	ID             string            `json:"id"`
	SessionID      string            `json:"session_id"`
	RootID         string            `json:"root_id"`
	RootLabel      string            `json:"root_label"`
	SessionName    string            `json:"session_name"`
	Turns          int64             `json:"turns"`
	ProfileTurns   map[string]int64  `json:"profile_turns,omitempty"`
	ActiveProfile  string            `json:"active_profile,omitempty"`
	DefaultProfile string            `json:"default_profile,omitempty"`
	Profiles       []string          `json:"profiles,omitempty"`
	ProfileNames   map[string]string `json:"profile_names,omitempty"`
	ModelDefault   string            `json:"model_default,omitempty"`
	// ModelFault is why the model a run of this session would use cannot be used (the error
	// names its file); set with ModelDefault empty and absent otherwise. A client must not
	// choose another model for the user: it asks for an explicit choice, which the model-default
	// route writes into the profile.
	ModelFault string `json:"model_fault,omitempty"`
	// ProfileETag is the entity tag of the profile file (catalog layout only), which the
	// model-default route takes back as If-Match.
	ProfileETag     string                             `json:"profile_etag,omitempty"`
	Models          *[]string                          `json:"models,omitempty"`
	ModelOptions    []modelOption                      `json:"model_options,omitempty"`
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
	// through is the end of the last record the usage accounts for: every byte before it is in
	// usage, directly or through the cache or baseline the scan started from. A rewrite of the
	// transcript that starts from this usage must see a file of exactly this size.
	through int64
	// wroteUsageCache is set when the scan replaced the usage cache file, which
	// changes the session version.
	wroteUsageCache bool
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
		version  string
		scanned  bool
	}
	candidates := make([]candidate, 0)
	versions := make([]string, 0)
	listed := make(map[string]struct{})
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
		if root.Layout != nil {
			// A session file under a name no route accepts is not listed; the log says why.
			r.warned.reportRoot(r.logger, root.ID, r.misnamed.Find(root, entries))
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
			config, err := r.listSettings(location, scans)
			if err != nil {
				var configErr *sessionfs.ConfigError
				if errors.As(err, &configErr) {
					r.skipped.report(r.logger, location.Reference, configErr.Error())
					continue
				}
				return httpapi.CatalogResponse{}, mapSessionError(err)
			}
			r.skipped.resolved(location.Reference)
			r.warned.report(r.logger, location.Reference, config.Profile, config.Warnings)
			listed[location.File] = struct{}{}
			sessionRuns := httpapi.SessionRunSummaries{}
			if runStates != nil {
				sessionRuns = runStates.SessionRuns(location.Reference, config.Profile)
			}
			version := location.Reference + ":" + sessionVersionForCatalog(location, config, r.catalog.MaxInlinePartBytes, sessionRuns) + profileTranscriptStamps(location, config)
			candidates = append(candidates, candidate{location: location, config: config, runs: sessionRuns, version: version})
			versions = append(versions, version)
		}
	}
	r.listSettingsCache.keepOnly(listed)
	sort.Strings(versions)
	etag := makeETag(strings.Join(versions, "|"))
	if httpapi.ETagMatches(ifNoneMatch, etag) {
		return httpapi.CatalogResponse{ETag: etag, NotModified: true}, nil
	}

	sessions := make([]sessionListRecord, 0, len(candidates))
	for i := range candidates {
		candidate := &candidates[i]
		if err := ctx.Err(); err != nil {
			return httpapi.CatalogResponse{}, err
		}
		shared, release, err := r.sharedStore(candidate.config)
		if err != nil {
			return httpapi.CatalogResponse{}, err
		}
		index, err := scanTranscriptIndex(shared, candidate.config)
		release()
		if err != nil {
			return httpapi.CatalogResponse{}, fmt.Errorf("read session transcript %q: %w", candidate.location.Reference, err)
		}
		candidate.scanned = index.wroteUsageCache
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
		// Only a scan that rewrote the usage cache changes what the first pass stamped.
		if candidate.scanned {
			candidate.version = candidate.location.Reference + ":" + sessionVersionForCatalog(candidate.location, candidate.config, r.catalog.MaxInlinePartBytes, candidate.runs) + profileTranscriptStamps(candidate.location, candidate.config)
		}
		finalVersions = append(finalVersions, candidate.version)
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
		shared, release, err := r.sharedStore(other)
		if err != nil {
			continue
		}
		index, err := scanTranscriptIndex(shared, other)
		release()
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
	shared, release, err := r.sharedStore(config)
	if err != nil {
		return httpapi.CatalogResponse{}, err
	}
	defer release()
	index, err := scanTranscriptIndex(shared, config)
	if err != nil {
		return httpapi.CatalogResponse{}, fmt.Errorf("read session transcript %q: %w", location.Reference, err)
	}
	messages, page, err := r.readMessagePage(ctx, config, shared.store, index, options)
	if err != nil {
		return httpapi.CatalogResponse{}, err
	}
	// A cursor page asks for older tail turns only; the caller already holds the
	// history block, and a page does not restate it.
	var historyRef *historyReference
	if options.Before == nil {
		if historyRef, err = r.historyReferenceFor(ctx, config, shared); err != nil {
			return httpapi.CatalogResponse{}, err
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

func (r *Resolver) readMessagePage(ctx context.Context, config sessionfs.SessionConfig, store *transcript.TurnStore, index transcriptIndex, options httpapi.SnapshotOptions) ([]json.RawMessage, sessionPage, error) {
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
	normalized = webui.CapMessages(r.attachSubagentSnapshots(ctx, config, normalized), r.catalog.MaxInlinePartBytes, nil)
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

// scanSnapshotRead reads the usage records of a scan. Tests replace it to force the
// retry on ErrTranscriptChanged; production always uses the snapshot read.
var scanSnapshotRead = (*transcript.TurnStore).ReadRecordsSnapshot

// scanTranscriptIndex builds the usage and turn index of the session transcript. A scan that
// raced a rewrite of the transcript is discarded and repeated from a fresh read of the usage
// cache and baseline, which describe the transcript that was rewritten away; a second change is
// reported to the caller.
func scanTranscriptIndex(shared *sharedSession, config sessionfs.SessionConfig) (transcriptIndex, error) {
	index, err := scanTranscriptIndexOnce(shared, config)
	if errors.Is(err, transcript.ErrTranscriptChanged) {
		index, err = scanTranscriptIndexOnce(shared, config)
	}
	return index, err
}

func scanTranscriptIndexOnce(shared *sharedSession, config sessionfs.SessionConfig) (transcriptIndex, error) {
	if shared == nil || shared.store == nil {
		return transcriptIndex{}, errors.New("shared transcript store is not initialized")
	}
	store := shared.store
	// Everything this scan trusts (the cache or baseline it starts from, the file it reads and
	// the file it stamps a new cache on) is tied to this one stat of the transcript.
	generation := "0:0:0"
	var stat0 fileStat
	existed := false
	if stat, err := statFile(config.MessagesPath); err == nil {
		stat0, generation, existed = stat, stat.generation(), true
	} else if !errors.Is(err, os.ErrNotExist) {
		return transcriptIndex{}, fmt.Errorf("stat transcript: %w", err)
	}
	// replaced says whether the transcript is no longer the file stat0 describes. A read that
	// failed because it used that file's offsets on another one is a race with a rewrite, not a
	// corrupt transcript, and is repeated.
	replaced := func(cause error) error {
		now, statErr := statFile(config.MessagesPath)
		if (statErr == nil && now.generation() != generation) || (statErr != nil && existed) {
			return fmt.Errorf("transcript replaced during the usage scan (%v): %w", cause, transcript.ErrTranscriptChanged)
		}
		return nil
	}
	starts, err := shared.readTurnStarts(generation)
	if err != nil {
		if raced := replaced(err); raced != nil {
			return transcriptIndex{}, raced
		}
		return transcriptIndex{}, fmt.Errorf("build transcript turn index: %w", err)
	}
	index := transcriptIndex{starts: starts}
	usageCache, cacheValid := readUsageCacheStateAt(config, stat0)
	fromBaseline := false
	if !cacheValid && existed {
		if entry, ok := shared.matchUsageBaseline(config, stat0); ok {
			usageCache = usageCacheFile{
				Transcript:      usageCheckpoint{Offset: entry.Offset},
				Summary:         entry.Summary,
				LastResponseKey: entry.LastResponseKey,
			}
			cacheValid, fromBaseline = true, true
		}
	}
	usageOffset := int64(0)
	if cacheValid {
		usageOffset = usageCache.Transcript.Offset
	}
	// The usage visitor accumulates into index, so a retry after a changed snapshot starts
	// from the cached summary again.
	var lastResponseKey string
	var turnCount, maxTurn int64
	var scanThrough int64
	scanAttempt := func() error {
		index.usage = webui.UsageSummary{}
		lastResponseKey = ""
		turnCount, maxTurn = 0, 0
		if cacheValid {
			index.usage = usageCache.Summary
			lastResponseKey = usageCache.LastResponseKey
			turnCount = index.usage.TurnCount
			maxTurn = turnCount
		}
		seenBatches := make(map[string]struct{})
		seenResponses := make(map[string]struct{})
		seenUsageKeys := make(map[string]struct{})
		scanThrough = usageOffset
		return scanSnapshotRead(store, usageOffset, 1<<63-1, func(offset int64, line []byte) error {
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
	}
	if err = scanAttempt(); err != nil {
		if raced := replaced(err); raced != nil && !errors.Is(err, transcript.ErrTranscriptChanged) {
			return transcriptIndex{}, raced
		}
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
		// Not a baseline either, so the totals of history a compaction removed are unknown.
		if backupInfo, backupErr := os.Stat(config.MessagesBackupPath); backupErr == nil && backupInfo.Size() > 0 {
			index.usage.HistoryPartial = true
		}
	}
	if scanThrough < usageOffset {
		scanThrough = usageOffset
	}
	index.through = scanThrough
	stat, err := statFile(config.MessagesPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && !existed {
			return index, nil
		}
		if errors.Is(err, os.ErrNotExist) {
			return transcriptIndex{}, fmt.Errorf("transcript removed during the usage scan: %w", transcript.ErrTranscriptChanged)
		}
		return transcriptIndex{}, fmt.Errorf("stat transcript: %w", err)
	}
	if stat.generation() != generation {
		// The file the cache or baseline was checked against is not the file that is there now,
		// so the scan may have read a mix of the two.
		return transcriptIndex{}, fmt.Errorf("transcript replaced during the usage scan: %w", transcript.ErrTranscriptChanged)
	}
	index.size = stat.Size
	index.generation = stat.generation()
	if (!cacheValid || fromBaseline || scanThrough > usageOffset) && scanThrough <= index.size {
		index.wroteUsageCache = shared.writeUsageCache(config, index.usage, scanThrough, lastResponseKey, generation)
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
		Turns:           turns,
		ActiveProfile:   config.Profile,
		DefaultProfile:  config.DefaultProfile,
		Profiles:        append([]string(nil), config.Profiles...),
		ProfileNames:    maps.Clone(config.ProfileNames),
		ModelDefault:    config.ModelDefault,
		ProfileETag:     profileETag(config.ProfilePath),
		FrontCompaction: append(json.RawMessage(nil), config.FrontCompaction...),
		Model: modelSummary{
			Provider: config.Model.Provider,
			Model:    config.Model.Name,
		},
	}
	if config.ModelFault != nil {
		item.ModelFault = config.ModelFault.Error()
	}
	if config.Models != nil {
		models := append([]string{}, config.Models...)
		item.Models = &models
		item.ModelOptions = listingModelOptions(config)
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
		config.ModelDefault,
		faultText(config.ModelFault),
		strings.Join(config.Profiles, "\x00"),
		profileNamesVersion(config.ProfileNames),
		fileStamp(config.MessagesPath),
		fileStamp(config.MessagesBackupPath),
		fileStamp(config.UsageCachePath),
		// A failed run changes the payload without touching the transcript.
		fileStamp(config.StoragePaths.RunErrorCache),
		fmt.Sprint(config.BackupTurns),
		fileStamp(config.SettingPath),
		fileStamps(config.ModelSourceFiles(config.ModelPath)),
		// The profile's entity tag is in the body, so an edit of the profile changes the version.
		fileStamp(config.ProfilePath),
	}
	for _, id := range config.Models {
		parts = append(parts, fileStamps(config.ModelSourceFiles(config.ModelPaths[id])))
	}
	return makeETag(strings.Join(parts, "|"))
}

// faultText is the text of err, empty for nil.
func faultText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// profileETag is the entity tag of the profile file, empty when there is none or it cannot be read.
func profileETag(path string) string {
	if path == "" {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return sessionfs.CatalogETag(data)
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

func makeETag(value string) string {
	hash := sha256.Sum256([]byte(value))
	return `"` + hex.EncodeToString(hash[:]) + `"`
}

// usageIsEmpty reports a summary with nothing to show: no completed responses,
// no turns, and no in-flight response.
func usageIsEmpty(usage webui.UsageSummary) bool {
	return usage.TurnCount == 0 && usage.CompletedResponses == 0 && !usage.HistoryPartial && usage.Current == nil
}
