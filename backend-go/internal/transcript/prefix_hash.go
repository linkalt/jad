package transcript

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
)

// HashFileRange returns the hex SHA-256 of bytes [start, end) of the file at
// path. The append-aware transcript caches use it to prove that the bytes they
// already processed are still the bytes on disk.
func HashFileRange(path string, start, end int64) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	return hashSection(file, start, end)
}

func hashSection(source io.ReaderAt, start, end int64) (string, error) {
	if start < 0 || end < start {
		return "", errors.New("transcript hash range is invalid")
	}
	hash := sha256.New()
	length := end - start
	if length > 0 {
		written, err := io.Copy(hash, io.NewSectionReader(source, start, length))
		if err != nil {
			return "", err
		}
		if written != length {
			return "", io.ErrUnexpectedEOF
		}
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
