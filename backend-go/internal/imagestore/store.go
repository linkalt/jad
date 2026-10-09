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
const RefPrefix = "stored-image://"

var (
	dataAnchor = []byte(`"image_url":"data:image/`)
	refAnchor  = []byte(`"image_url":"` + RefPrefix)
	// extensions maps the image types that are stored to a file extension. Other types
	// (svg, tiff, ...) stay inline: the file is later served by extension, and these four
	// can be checked by their first bytes.
	extensions = map[string]string{"image/png": "png", "image/jpeg": "jpg", "image/webp": "webp", "image/gif": "gif"}
	mimeByExt  = map[string]string{"png": "image/png", "jpg": "image/jpeg", "webp": "image/webp", "gif": "image/gif"}
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
		if !bytes.Contains(item, refAnchor) {
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
		at := bytes.Index(item[searchFrom:], refAnchor)
		if at < 0 {
			break
		}
		anchorStart := searchFrom + at
		valueStart := anchorStart + len(`"image_url":"`)
		end := bytes.IndexByte(item[valueStart:], '"')
		if end < 0 {
			break
		}
		valueEnd := valueStart + end
		searchFrom = valueEnd
		name := string(item[valueStart+len(RefPrefix) : valueEnd])
		data, mime, err := s.Open(name)
		if err != nil {
			stats.Missing++
			start, stop, found := partBounds(item, anchorStart, valueEnd)
			if !found || start < copied {
				// The part cannot be told apart, so the value becomes a one-pixel image.
				out = append(out, item[copied:valueStart]...)
				out = append(out, onePixelPNG...)
				copied = valueEnd
				continue
			}
			out = append(out, item[copied:start]...)
			out = append(out, unavailableImage...)
			copied = stop
			continue
		}
		out = append(out, item[copied:valueStart]...)
		out = append(out, "data:"...)
		out = append(out, mime...)
		out = append(out, ";base64,"...)
		out = base64.StdEncoding.AppendEncode(out, data)
		copied = valueEnd
		stats.Restored++
	}
	return append(out, item[copied:]...), stats
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
	extension, known := extensions[strings.TrimPrefix(header, "data:")]
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
	if http.DetectContentType(data) != strings.TrimPrefix(header, "data:") {
		return "", false
	}
	sum := sha256.Sum256(data)
	name := hex.EncodeToString(sum[:]) + "." + extension
	path := filepath.Join(s.options.Dir, name)

	s.mu.Lock()
	defer s.mu.Unlock()
	if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() && info.Size() == size {
		return RefPrefix + name, true
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
	return RefPrefix + name, true
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
