package imageresize

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"sync"
)

// Sources an image can come from. A tool result is "mcp" or "native" by the tool that
// produced it; "user" is an image in a user message.
const (
	SourceMCP    = "mcp"
	SourceNative = "native"
	SourceUser   = "user"
)

// Stats says what one Apply call did.
type Stats struct {
	// Images is the number of images of an applicable source that were looked at.
	Images int
	// Resized is how many of them were replaced.
	Resized int
	// BytesBefore and BytesAfter count the data URL text of the resized images only.
	BytesBefore, BytesAfter int
}

// Resizer rewrites the images of one request. It is safe for concurrent use. The results
// are memoised by the hash of the image, so a long history costs one resize per image and
// every round sends identical bytes (which keeps the provider's prompt cache valid).
type Resizer struct {
	options   Options
	appliesTo map[string]bool

	mu    sync.Mutex
	cache map[[sha256.Size]byte]string // "" means: leave the image as it is
	order [][sha256.Size]byte
}

// New returns a Resizer for the given limits and sources.
func New(options Options, appliesTo []string) *Resizer {
	sources := make(map[string]bool, len(appliesTo))
	for _, source := range appliesTo {
		sources[source] = true
	}
	return &Resizer{options: options, appliesTo: sources, cache: make(map[[sha256.Size]byte]string)}
}

// SourceOf names the source of a function call item ("mcp" or "native"), or "" when the
// tool is unknown.
type SourceOf func(functionCall json.RawMessage) string

// Apply returns the history to send. It returns the argument itself when nothing changes,
// and otherwise a new slice that shares every unchanged item. It never changes an item
// in place, so the transcript's own bytes stay as they were.
func (r *Resizer) Apply(history [][]byte, sourceOf SourceOf) ([][]byte, Stats) {
	var stats Stats
	if r == nil || len(r.appliesTo) == 0 {
		return history, stats
	}
	var calls map[string]json.RawMessage
	var result [][]byte
	for index, item := range history {
		if !bytes.Contains(item, []byte(`"input_image"`)) && !bytes.Contains(item, []byte(`"function_call"`)) {
			continue
		}
		var header struct {
			Type   string `json:"type"`
			Role   string `json:"role"`
			CallID string `json:"call_id"`
		}
		if json.Unmarshal(item, &header) != nil {
			continue
		}
		var source string
		switch {
		case header.Type == "function_call":
			if calls == nil {
				calls = make(map[string]json.RawMessage)
			}
			calls[header.CallID] = item
			continue
		case header.Type == "function_call_output":
			if call, found := calls[header.CallID]; found && sourceOf != nil {
				source = sourceOf(call)
			}
		case (header.Type == "message" || header.Type == "") && header.Role == "user":
			source = SourceUser
		}
		if !r.appliesTo[source] || !bytes.Contains(item, []byte(`"input_image"`)) {
			continue
		}
		rewritten, changed := r.rewriteItem(item, &stats)
		if !changed {
			continue
		}
		if result == nil {
			result = make([][]byte, len(history))
			copy(result, history)
		}
		result[index] = rewritten
	}
	if result == nil {
		return history, stats
	}
	return result, stats
}

// rewriteItem resizes the input_image parts of a function_call_output (its output) or of
// a message (its content). It reports false when no part changed.
func (r *Resizer) rewriteItem(item []byte, stats *Stats) ([]byte, bool) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(item, &fields) != nil {
		return nil, false
	}
	key := "output"
	if _, isMessage := fields["content"]; isMessage {
		key = "content"
	}
	var parts []json.RawMessage
	if json.Unmarshal(fields[key], &parts) != nil {
		return nil, false
	}
	changed := false
	for index, part := range parts {
		var partFields map[string]json.RawMessage
		if json.Unmarshal(part, &partFields) != nil {
			continue
		}
		var partType, url string
		if json.Unmarshal(partFields["type"], &partType) != nil || partType != "input_image" {
			continue
		}
		if json.Unmarshal(partFields["image_url"], &url) != nil || url == "" {
			continue
		}
		stats.Images++
		replacement, ok := r.resized(url)
		if !ok {
			continue
		}
		encoded, err := json.Marshal(replacement)
		if err != nil {
			continue
		}
		partFields["image_url"] = encoded
		rebuilt, err := json.Marshal(partFields)
		if err != nil {
			continue
		}
		parts[index] = rebuilt
		stats.Resized++
		stats.BytesBefore += len(url)
		stats.BytesAfter += len(replacement)
		changed = true
	}
	if !changed {
		return nil, false
	}
	rebuiltParts, err := json.Marshal(parts)
	if err != nil {
		return nil, false
	}
	fields[key] = rebuiltParts
	rebuilt, err := json.Marshal(fields)
	if err != nil {
		return nil, false
	}
	return rebuilt, true
}

// resized returns the replacement data URL, or false to keep the image.
func (r *Resizer) resized(url string) (string, bool) {
	key := sha256.Sum256([]byte(url))
	r.mu.Lock()
	cached, found := r.cache[key]
	r.mu.Unlock()
	if found {
		return cached, cached != ""
	}
	replacement, err := resizeDataURL(url, r.options)
	if err != nil {
		replacement = ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.cache[key]; !exists {
		if r.options.CacheEntries > 0 && len(r.order) >= r.options.CacheEntries {
			delete(r.cache, r.order[0])
			r.order = r.order[1:]
		}
		if r.options.CacheEntries > 0 {
			r.cache[key] = replacement
			r.order = append(r.order, key)
		}
	}
	return replacement, replacement != ""
}

// Resize returns the replacement data URL for one image that comes from source, or false
// to keep it as it is: the source is not listed in applies_to, or the image needs no
// change or cannot be decoded. It is the single-image form of Apply, for a tool that
// builds its own request.
func (r *Resizer) Resize(source, dataURL string) (string, bool) {
	if r == nil || !r.appliesTo[source] {
		return "", false
	}
	return r.resized(dataURL)
}
