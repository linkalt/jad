// Package imagestore keeps large images out of messages.jsonl. An image that a tool
// returned as a base64 data URL is written once to a directory as <sha256>.<ext>, and the
// transcript keeps a short reference in its place. Before a request goes to the provider the
// references are replaced by data URLs again, so the model sees the same image.
//
// Both directions work on the raw bytes of one item and only replace the value of an
// "image_url" key, so every other byte of the item keeps its place.
package imagestore

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// RefPrefix starts every reference the transcript holds in place of a data URL.
// RawRefPrefix does the same for a bare base64 string, the form some providers use for a
// generated image (the value has no "data:" header, so the reference must say so).
const (
	RefPrefix    = "stored-image://"
	RawRefPrefix = "stored-image-b64://"
)

var (
	dataAnchor = []byte(`"image_url":"data:image/`)
	// extensions maps the image types that are stored to a file extension. Other types
	// (svg, tiff, ...) stay inline: the file is later served by extension, and these four
	// can be checked by their first bytes.
	extensions = map[string]string{"image/png": "png", "image/jpeg": "jpg", "image/webp": "webp", "image/gif": "gif"}
	mimeByExt  = map[string]string{"png": "image/png", "jpg": "image/jpeg", "webp": "image/webp", "gif": "image/gif"}
	// keyedRef finds the start of a string value whose text begins like a reference.
	keyedRef = []byte(`":"stored-image`)
	// rawMagic maps the first base64 characters of each stored image type to its type, so
	// a bare base64 value can be told from other text.
	rawMagic = []struct{ prefix, mime string }{
		{"iVBORw0KGgo", "image/png"}, {"/9j/", "image/jpeg"}, {"UklGR", "image/webp"}, {"R0lGOD", "image/gif"},
	}
	// plainItemTypes are response items whose text is never rewritten by key: a message
	// holds what a person or the model wrote, and a function call holds model text.
	plainItemTypes = map[string]struct{}{"message": {}, "reasoning": {}, "function_call": {}, "function_call_output": {}}
	// unavailableImage is what a part becomes when its file is gone or damaged.
	unavailableImage = []byte(`{"type":"input_text","text":"[stored image unavailable]"}`)
)

// Options configures a Store. Every value comes from the image_store setting.
type Options struct {
	// Dir is the directory the files live in. It is created on the first write.
	Dir string
	// MinBytes: a smaller decoded image stays inline.
	MinBytes int64
	// MaxFileBytes: a larger decoded image stays inline.
	MaxFileBytes int64
	// MaxTotalBytes: once the directory holds this much, new images stay inline.
	MaxTotalBytes int64
}

// Stats says what one call did.
type Stats struct {
	// Stored is the number of images written or found again by hash and replaced by a reference.
	Stored int
	// Restored is the number of references replaced by their data URL.
	Restored int
	// Missing is the number of references whose file could not be read.
	Missing int
}

// Store is safe for concurrent use. A nil *Store does nothing.
type Store struct {
	options Options

	mu      sync.Mutex
	scanned bool
	total   int64
}

// New returns nil when no directory is configured.
func New(options Options) *Store {
	if options.Dir == "" {
		return nil
	}
	return &Store{options: options}
}

// Externalize replaces every data URL image of the item by a reference. It returns the item
// itself when nothing changed. An image that cannot be stored stays where it is.
func (s *Store) Externalize(item []byte) ([]byte, Stats) {
	var stats Stats
	if s == nil || !bytes.Contains(item, dataAnchor) {
		return item, stats
	}
	var out []byte
	copied := 0
	searchFrom := 0
	for {
		at := bytes.Index(item[searchFrom:], dataAnchor)
		if at < 0 {
			break
		}
		valueStart := searchFrom + at + len(`"image_url":"`)
		end := bytes.IndexByte(item[valueStart:], '"')
		if end < 0 {
			break
		}
		valueEnd := valueStart + end
		searchFrom = valueEnd
		ref, ok := s.put(string(item[valueStart:valueEnd]))
		if !ok {
			continue
		}
		out = append(out, item[copied:valueStart]...)
		out = append(out, ref...)
		copied = valueEnd
		stats.Stored++
	}
	if stats.Stored == 0 {
		return item, stats
	}
	return append(out, item[copied:]...), stats
}

// ExternalizeAll applies Externalize to tool outputs about to be persisted.
func (s *Store) ExternalizeAll(items []json.RawMessage) ([]json.RawMessage, Stats) {
	var total Stats
	if s == nil {
		return items, total
	}
	var result []json.RawMessage
	for index, item := range items {
		replaced, stats := s.Externalize(item)
		if stats.Stored == 0 {
			continue
		}
		if result == nil {
			result = append([]json.RawMessage(nil), items...)
		}
		result[index] = replaced
		total.Stored += stats.Stored
	}
	if result == nil {
		return items, total
	}
	return result, total
}

// Rehydrate returns the history with every reference replaced by its data URL. It returns
// the argument itself when nothing changes, and otherwise a new slice that shares every
// unchanged item. A reference whose file is missing becomes a short text part, so one lost
// file never fails a run.
func (s *Store) Rehydrate(history [][]byte) ([][]byte, Stats) {
	var stats Stats
	if s == nil {
		return history, stats
	}
	var result [][]byte
	for index, item := range history {
		if !bytes.Contains(item, keyedRef) {
			continue
		}
		replaced, itemStats := s.rehydrateItem(item)
		stats.Restored += itemStats.Restored
		stats.Missing += itemStats.Missing
		if result == nil {
			result = append([][]byte(nil), history...)
		}
		result[index] = replaced
	}
	if result == nil {
		return history, stats
	}
	return result, stats
}

func (s *Store) rehydrateItem(item []byte) ([]byte, Stats) {
	var stats Stats
	var out []byte
	copied := 0
	searchFrom := 0
	for {
		at := bytes.Index(item[searchFrom:], keyedRef)
		if at < 0 {
			break
		}
		anchorStart := searchFrom + at
		valueStart := anchorStart + len(`":"`)
		end := bytes.IndexByte(item[valueStart:], '"')
		if end < 0 {
			break
		}
		valueEnd := valueStart + end
		searchFrom = valueEnd
		value := string(item[valueStart:valueEnd])
		raw := strings.HasPrefix(value, RawRefPrefix)
		name := strings.TrimPrefix(strings.TrimPrefix(value, RefPrefix), RawRefPrefix)
		if !raw && !strings.HasPrefix(value, RefPrefix) {
			continue
		}
		imageURLKey := bytes.HasSuffix(item[:anchorStart+1], []byte(`"image_url"`)) && !raw
		if !imageURLKey {
			// Any other key holds a generated image, which only an item the provider
			// filled itself carries. Text a person or the model wrote is left alone.
			if _, valid := ValidName(name); !valid || plainItem(item) {
				continue
			}
		}
		data, mime, err := s.Open(name)
		if err != nil {
			stats.Missing++
			if imageURLKey {
				start, stop, found := partBounds(item, anchorStart-len(`"image_url`), valueEnd)
				if found && start >= copied {
					out = append(out, item[copied:start]...)
					out = append(out, unavailableImage...)
					copied = stop
					continue
				}
			}
			// The value becomes a one-pixel image, in the form the original had.
			out = append(out, item[copied:valueStart]...)
			if raw {
				out = append(out, onePixelPNG[len("data:image/png;base64,"):]...)
			} else {
				out = append(out, onePixelPNG...)
			}
			copied = valueEnd
			continue
		}
		out = append(out, item[copied:valueStart]...)
		if !raw {
			out = append(out, "data:"...)
			out = append(out, mime...)
			out = append(out, ";base64,"...)
		}
		out = base64.StdEncoding.AppendEncode(out, data)
		copied = valueEnd
		stats.Restored++
	}
	return append(out, item[copied:]...), stats
}

// ExternalizeOutput applies the store to the output array of a completed response. Only
// items that are neither a message, reasoning nor a function call are touched: they are
// the items a provider fills itself (image_generation_call, openrouter:image_generation,
// ...), and a generated image sits in one of their string values under a key the store
// does not know. Any such value that is a data URL, or bare base64 that starts like a
// png, jpeg, webp or gif, is written to the store and replaced by a reference. It returns
// the argument itself when nothing changed, and keeps the bytes of every other item.
func (s *Store) ExternalizeOutput(output json.RawMessage) (json.RawMessage, Stats) {
	var stats Stats
	if s == nil {
		return output, stats
	}
	var items []json.RawMessage
	if json.Unmarshal(output, &items) != nil {
		return output, stats
	}
	changed := false
	for index, item := range items {
		replaced, itemStats := s.ExternalizeItem(item)
		if itemStats.Stored == 0 {
			continue
		}
		items[index] = replaced
		stats.Stored += itemStats.Stored
		changed = true
	}
	if !changed {
		return output, stats
	}
	var out bytes.Buffer
	out.WriteByte('[')
	for index, item := range items {
		if index > 0 {
			out.WriteByte(',')
		}
		out.Write(item)
	}
	out.WriteByte(']')
	return out.Bytes(), stats
}

// plainItem reports whether the item is a message, reasoning or function call, by its
// top-level "type".
func plainItem(item []byte) bool {
	var head struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(item, &head) != nil {
		return true
	}
	_, plain := plainItemTypes[head.Type]
	return plain || head.Type == ""
}

// ExternalizeItem is ExternalizeOutput for one item. An item that is a message, reasoning
// or a function call, or has no type, comes back unchanged.
func (s *Store) ExternalizeItem(item []byte) ([]byte, Stats) {
	var stats Stats
	if s == nil {
		return item, stats
	}
	var head struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(item, &head) != nil || head.Type == "" {
		return item, stats
	}
	if _, plain := plainItemTypes[head.Type]; plain {
		return item, stats
	}
	var compact bytes.Buffer
	if json.Compact(&compact, item) != nil {
		return item, stats
	}
	return s.externalizeKeyed(compact.Bytes())
}

// minKeyedValue: a string shorter than this is never an image worth storing, so it is
// not decoded to find out.
const minKeyedValue = 64

// externalizeKeyed replaces every image string value of one item, whatever its key.
func (s *Store) externalizeKeyed(item []byte) ([]byte, Stats) {
	var stats Stats
	var out []byte
	copied := 0
	searchFrom := 0
	for {
		at := bytes.Index(item[searchFrom:], []byte(`":"`))
		if at < 0 {
			break
		}
		valueStart := searchFrom + at + len(`":"`)
		searchFrom = valueStart
		end := bytes.IndexByte(item[valueStart:], '"')
		if end < 0 {
			break
		}
		value := item[valueStart : valueStart+end]
		if len(value) < minKeyedValue || bytes.IndexByte(value, '\\') >= 0 {
			continue
		}
		var reference string
		if bytes.HasPrefix(value, []byte("data:image/")) {
			ref, ok := s.put(string(value))
			if !ok {
				continue
			}
			reference = ref
		} else if mime := rawImageType(value); mime != "" {
			name, ok := s.putPayload(mime, string(value))
			if !ok {
				continue
			}
			reference = RawRefPrefix + name
		} else {
			continue
		}
		out = append(out, item[copied:valueStart]...)
		out = append(out, reference...)
		copied = valueStart + end
		searchFrom = copied
		stats.Stored++
	}
	if stats.Stored == 0 {
		return item, stats
	}
	return append(out, item[copied:]...), stats
}

// rawImageType tells by the first base64 characters which image type a bare base64
// value would be, or "" when it does not start like one.
func rawImageType(value []byte) string {
	for _, magic := range rawMagic {
		if bytes.HasPrefix(value, []byte(magic.prefix)) {
			return magic.mime
		}
	}
	return ""
}

// RefNames lists the names of the stored images an item refers to, under any key, in
// order and without repeats. Only valid names are listed.
func RefNames(item []byte) []string {
	var names []string
	searchFrom := 0
	for {
		at := bytes.Index(item[searchFrom:], keyedRef)
		if at < 0 {
			return names
		}
		valueStart := searchFrom + at + len(`":"`)
		end := bytes.IndexByte(item[valueStart:], '"')
		if end < 0 {
			return names
		}
		searchFrom = valueStart + end
		value := string(item[valueStart:searchFrom])
		name := strings.TrimPrefix(strings.TrimPrefix(value, RefPrefix), RawRefPrefix)
		if name == value {
			continue
		}
		if _, valid := ValidName(name); !valid {
			continue
		}
		duplicate := false
		for _, known := range names {
			duplicate = duplicate || known == name
		}
		if !duplicate {
			names = append(names, name)
		}
	}
}

// partBounds finds the flat JSON object {"type":"input_image",...} around a reference. It
// reports false when the surroundings are not that shape.
func partBounds(item []byte, anchorStart, valueEnd int) (start, stop int, found bool) {
	start = bytes.LastIndexByte(item[:anchorStart], '{')
	if start < 0 || bytes.ContainsAny(item[start+1:anchorStart], "{}[]") {
		return 0, 0, false
	}
	if !bytes.HasPrefix(item[start:], []byte(`{"type":"input_image"`)) {
		return 0, 0, false
	}
	close := bytes.IndexByte(item[valueEnd:], '}')
	if close < 0 || bytes.ContainsAny(item[valueEnd:valueEnd+close], "{[]") {
		return 0, 0, false
	}
	return start, valueEnd + close + 1, true
}

// put stores one data URL and returns the reference text, or false to keep it inline.
func (s *Store) put(dataURL string) (string, bool) {
	header, payload, found := strings.Cut(dataURL, ";base64,")
	if !found {
		return "", false
	}
	name, ok := s.putPayload(strings.TrimPrefix(header, "data:"), payload)
	if !ok {
		return "", false
	}
	return RefPrefix + name, true
}

// putPayload stores one base64 image of the given type and returns its file name.
func (s *Store) putPayload(mime, payload string) (string, bool) {
	extension, known := extensions[mime]
	if !known {
		return "", false
	}
	if int64(len(payload))/4*3 > s.options.MaxFileBytes+3 {
		return "", false
	}
	data, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return "", false
	}
	size := int64(len(data))
	if size < s.options.MinBytes || size > s.options.MaxFileBytes || size == 0 {
		return "", false
	}
	if http.DetectContentType(data) != mime {
		return "", false
	}
	sum := sha256.Sum256(data)
	name := hex.EncodeToString(sum[:]) + "." + extension
	path := filepath.Join(s.options.Dir, name)

	s.mu.Lock()
	defer s.mu.Unlock()
	if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() && info.Size() == size {
		return name, true
	}
	if !s.scanned {
		s.total = directorySize(s.options.Dir)
		s.scanned = true
	}
	if s.total+size > s.options.MaxTotalBytes {
		return "", false
	}
	if err := writeAtomic(s.options.Dir, path, data); err != nil {
		return "", false
	}
	s.total += size
	return name, true
}

// Open reads one stored file by its name (<64 hex>.<ext>). It never takes a path.
func (s *Store) Open(name string) ([]byte, string, error) {
	if s == nil {
		return nil, "", errors.New("image store is off")
	}
	extension, ok := ValidName(name)
	if !ok {
		return nil, "", fmt.Errorf("invalid image name %q", name)
	}
	path := filepath.Join(s.options.Dir, name)
	// Lstat does not follow a link; the opened file must be the very file it described, so
	// a link swapped in between cannot lead outside the directory.
	linkInfo, err := os.Lstat(path)
	if err != nil {
		return nil, "", err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, "", err
	}
	if !linkInfo.Mode().IsRegular() || !info.Mode().IsRegular() || !os.SameFile(linkInfo, info) || info.Size() > s.options.MaxFileBytes {
		return nil, "", errors.New("stored image is not a regular file within the size limit")
	}
	data := make([]byte, info.Size())
	if _, err := readFull(file, data); err != nil {
		return nil, "", err
	}
	mime := mimeByExt[extension]
	if http.DetectContentType(data) != mime {
		return nil, "", errors.New("stored image content does not match its name")
	}
	return data, mime, nil
}

// ValidName reports whether name is <64 lowercase hex>.<png|jpg|webp|gif> and returns the extension.
func ValidName(name string) (string, bool) {
	stem, extension, found := strings.Cut(name, ".")
	if !found || len(stem) != sha256.Size*2 {
		return "", false
	}
	if _, known := mimeByExt[extension]; !known {
		return "", false
	}
	for _, c := range []byte(stem) {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return "", false
		}
	}
	return extension, true
}

// MIMEType returns the content type of a valid name.
func MIMEType(name string) string {
	extension, ok := ValidName(name)
	if !ok {
		return ""
	}
	return mimeByExt[extension]
}

func readFull(file *os.File, buffer []byte) (int, error) {
	read := 0
	for read < len(buffer) {
		n, err := file.Read(buffer[read:])
		read += n
		if err != nil {
			if read == len(buffer) {
				return read, nil
			}
			return read, err
		}
	}
	return read, nil
}

func writeAtomic(dir, path string, data []byte) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	_, writeErr := temp.Write(data)
	closeErr := temp.Close()
	if writeErr != nil || closeErr != nil {
		_ = os.Remove(tempName)
		return errors.Join(writeErr, closeErr)
	}
	if err := os.Chmod(tempName, 0o600); err != nil {
		_ = os.Remove(tempName)
		return err
	}
	if err := os.Rename(tempName, path); err != nil {
		_ = os.Remove(tempName)
		return err
	}
	return nil
}

// directorySize adds up the stored files; temporary and foreign files are not counted.
func directorySize(dir string) int64 {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	var total int64
	for _, entry := range entries {
		if _, ok := ValidName(entry.Name()); !ok || entry.Type()&fs.ModeType != 0 {
			continue
		}
		if info, err := entry.Info(); err == nil {
			total += info.Size()
		}
	}
	return total
}

// onePixelPNG is a complete data URL of a transparent 1x1 PNG, the last resort for a
// reference whose part cannot be replaced as a whole.
var onePixelPNG = []byte("data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg==")
