package native

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"example.com/agent-harness/backend-go/internal/sessionfs"
)

// FetchDocument is a fetched text document and its stable metadata.
type FetchDocument struct {
	ID          string
	URL         string
	FinalURL    string
	Title       string
	ContentType string
	Status      int
	FetchedAt   string
	Markdown    string
}

type storedFetchMetadata struct {
	ID          string `json:"id"`
	URL         string `json:"url"`
	FinalURL    string `json:"finalUrl"`
	Title       string `json:"title,omitempty"`
	ContentType string `json:"contentType"`
	Status      int    `json:"status"`
	FetchedAt   string `json:"fetchedAt"`
}

// FetchPage is one character-bounded page of a stored document.
type FetchPage struct {
	Document   FetchDocument
	Content    string
	NextCursor string
}

// FetchStoreError marks an invalid or unavailable document cursor, or a
// configured storage-capacity rejection.
type FetchStoreError struct {
	Message string
}

func (e *FetchStoreError) Error() string { return e.Message }

var fetchCursorPattern = regexp.MustCompile(`(?i)^([0-9a-f]{8}-(?:[0-9a-f]{4}-){3}[0-9a-f]{12})\.([0-9]+)$`)

// FetchStore persists fetched documents as a Markdown body and a compact
// metadata sidecar. A shared store serializes quota checks and creates.
type FetchStore struct {
	directory string
	mu        sync.Mutex
}

func NewFetchStore(directory string) *FetchStore {
	return &FetchStore{directory: directory}
}

// Create stores a fetched document, enforcing per-document and aggregate quotas.
func (s *FetchStore) Create(document FetchDocument, policy sessionfs.NativeFetchSettings) (FetchDocument, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	id, err := newFetchID()
	if err != nil {
		return FetchDocument{}, err
	}
	document.ID = id
	document.FetchedAt = time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	metadata := storedFetchMetadata{
		ID: id, URL: document.URL, FinalURL: document.FinalURL, Title: document.Title,
		ContentType: document.ContentType, Status: document.Status, FetchedAt: document.FetchedAt,
	}
	metadataBytes, err := marshalFetchMetadata(metadata)
	if err != nil {
		return FetchDocument{}, err
	}
	storedBytes := int64(len(document.Markdown)) + int64(len(metadataBytes))
	if storedBytes > policy.MaxStorageBytes {
		return FetchDocument{}, &FetchStoreError{Message: "fetched document exceeds configured storage capacity"}
	}
	if err := os.MkdirAll(s.directory, 0o755); err != nil {
		return FetchDocument{}, err
	}
	usedBytes, err := fetchDirectoryBytes(s.directory)
	if err != nil {
		return FetchDocument{}, err
	}
	if usedBytes+storedBytes > policy.MaxStorageBytes {
		return FetchDocument{}, &FetchStoreError{Message: "fetch storage is full"}
	}
	markdownPath := filepath.Join(s.directory, id+".md")
	if err := atomicFetchWrite(markdownPath, []byte(document.Markdown)); err != nil {
		return FetchDocument{}, err
	}
	if err := atomicFetchWrite(filepath.Join(s.directory, id+".json"), metadataBytes); err != nil {
		_ = os.Remove(markdownPath)
		return FetchDocument{}, err
	}
	return document, nil
}

// Page returns a stored page addressed by an opaque id.offset cursor.
func (s *FetchStore) Page(cursor string, limit int64) (FetchPage, error) {
	id, offset, err := parseFetchCursor(cursor)
	if err != nil {
		return FetchPage{}, err
	}
	if limit < 1 {
		limit = 1
	}
	metadataPath := filepath.Join(s.directory, id+".json")
	markdownPath := filepath.Join(s.directory, id+".md")
	metadataBytes, err := os.ReadFile(metadataPath)
	if err != nil {
		return FetchPage{}, &FetchStoreError{Message: "fetch cursor was not found"}
	}
	markdownBytes, err := os.ReadFile(markdownPath)
	if err != nil {
		return FetchPage{}, &FetchStoreError{Message: "fetch cursor was not found"}
	}
	var metadata storedFetchMetadata
	if err := json.Unmarshal(metadataBytes, &metadata); err != nil {
		return FetchPage{}, &FetchStoreError{Message: "fetch cursor was not found"}
	}
	markdown := string(markdownBytes)
	if metadata.ID != id || offset > int64(utf16Length(markdown)) {
		return FetchPage{}, &FetchStoreError{Message: "fetch cursor is invalid"}
	}
	content, consumed := sliceFetchCharacters(markdown, offset, limit)
	nextOffset := offset + consumed
	page := FetchPage{
		Document: FetchDocument{
			ID: metadata.ID, URL: metadata.URL, FinalURL: metadata.FinalURL, Title: metadata.Title,
			ContentType: metadata.ContentType, Status: metadata.Status, FetchedAt: metadata.FetchedAt,
			Markdown: markdown,
		},
		Content: content,
	}
	if nextOffset < int64(utf16Length(markdown)) {
		page.NextCursor = fetchCursor(metadata.ID, nextOffset)
	}
	return page, nil
}

// CreatePage stores the document and returns its first page.
func (s *FetchStore) CreatePage(document FetchDocument, limit int64, policy sessionfs.NativeFetchSettings) (FetchPage, error) {
	if limit < 1 {
		limit = 1
	}
	stored, err := s.Create(document, policy)
	if err != nil {
		return FetchPage{}, err
	}
	content, consumed := sliceFetchCharacters(stored.Markdown, 0, limit)
	page := FetchPage{Document: stored, Content: content}
	if consumed < int64(utf16Length(stored.Markdown)) {
		page.NextCursor = fetchCursor(stored.ID, consumed)
	}
	return page, nil
}

func parseFetchCursor(cursor string) (string, int64, error) {
	match := fetchCursorPattern.FindStringSubmatch(cursor)
	if len(match) != 3 {
		return "", 0, &FetchStoreError{Message: "fetch cursor is invalid"}
	}
	offset, err := strconv.ParseInt(match[2], 10, 64)
	if err != nil || offset > 9_007_199_254_740_991 {
		return "", 0, &FetchStoreError{Message: "fetch cursor is invalid"}
	}
	return strings.ToLower(match[1]), offset, nil
}

func fetchCursor(id string, offset int64) string {
	return fmt.Sprintf("%s.%d", id, offset)
}

// sliceFetchCharacters uses the TypeScript cursor's UTF-16 units while keeping
// surrogate pairs intact so a page never loses part of a Unicode character.
func sliceFetchCharacters(value string, offset, limit int64) (string, int64) {
	units := utf16.Encode([]rune(value))
	start := offset
	if start > int64(len(units)) {
		start = int64(len(units))
	}
	end := start + limit
	if end < start || end > int64(len(units)) {
		end = int64(len(units))
	}
	if end > start && end < int64(len(units)) && units[end-1] >= 0xd800 && units[end-1] <= 0xdbff && units[end] >= 0xdc00 && units[end] <= 0xdfff {
		end++
	}
	return string(utf16.Decode(units[int(start):int(end)])), end - start
}

func fetchDirectoryBytes(directory string) (int64, error) {
	entries, err := os.ReadDir(directory)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var total int64
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			return 0, err
		}
		if info.Mode().IsRegular() {
			total += info.Size()
		}
	}
	return total, nil
}

func atomicFetchWrite(path string, contents []byte) error {
	directory := filepath.Dir(path)
	file, err := os.CreateTemp(directory, ".fetch-*.tmp")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := io.Copy(file, bytes.NewReader(contents)); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

func marshalFetchMetadata(metadata storedFetchMetadata) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(metadata); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte{'\n'}), nil
}

func newFetchID() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	bytes[6] = (bytes[6] & 0x0f) | 0x40
	bytes[8] = (bytes[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(bytes[:])
	return encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:], nil
}
