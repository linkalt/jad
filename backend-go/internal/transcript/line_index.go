package transcript

import (
	"bufio"
	"crypto/sha256"
	"errors"
	"io"
	"os"
)

// lineIndexTailBytes is the window before the indexed end whose hash proves the
// file was not rewritten in place under the index.
const lineIndexTailBytes int64 = 4096

// lineIndex holds the SHA-256 of every complete physical line of messages.jsonl
// up to size, so a commit can test "is this line already present" without
// reading the whole file. It is only touched while Journal.mu is held.
//
// It stays valid while the file keeps its identity, has not shrunk, no whole-file
// rewrite happened through the Journal (generation) and the bytes just before
// size still hash as before. Appends made through the Journal are picked up
// by reading only the bytes after size. A change in the middle of the file by
// another process is not detected; the Journal already requires a single writer.
type lineIndex struct {
	hashes     map[[sha256.Size]byte]struct{}
	file       os.FileInfo
	generation uint64
	size       int64
	tailHash   string
}

// containsLine reports whether a line with the given hash is in the transcript
// behind file. The caller holds j.mu and has repaired any partial last line.
func (j *Journal) containsLine(file *os.File, target [sha256.Size]byte) (bool, error) {
	info, err := file.Stat()
	if err != nil {
		return false, err
	}
	index := j.index
	if index == nil || !index.usable(file, info, j.generation) {
		index = &lineIndex{hashes: make(map[[sha256.Size]byte]struct{}), file: info, generation: j.generation}
	}
	if err := index.extend(file, info.Size()); err != nil {
		j.index = nil
		return false, err
	}
	j.index = index
	_, present := index.hashes[target]
	return present, nil
}

func (index *lineIndex) usable(file *os.File, info os.FileInfo, generation uint64) bool {
	if index.generation != generation || !os.SameFile(index.file, info) || info.Size() < index.size {
		return false
	}
	tailHash, err := tailWindowHash(file, index.size)
	return err == nil && tailHash == index.tailHash
}

func tailWindowHash(file *os.File, end int64) (string, error) {
	start := end - lineIndexTailBytes
	if start < 0 {
		start = 0
	}
	return hashSection(file, start, end)
}

// extend indexes the complete lines between size and end. A partial last line
// is left for the next call.
func (index *lineIndex) extend(file *os.File, end int64) error {
	if end > index.size {
		reader := bufio.NewReader(io.NewSectionReader(file, index.size, end-index.size))
		lineHash := sha256.New()
		var pending int64
		for {
			chunk, err := reader.ReadSlice('\n')
			pending += int64(len(chunk))
			if len(chunk) > 0 {
				lineHash.Write(chunk)
			}
			if err == nil {
				var key [sha256.Size]byte
				lineHash.Sum(key[:0])
				index.hashes[key] = struct{}{}
				index.size += pending
				pending = 0
				lineHash.Reset()
				continue
			}
			if errors.Is(err, bufio.ErrBufferFull) {
				continue
			}
			if errors.Is(err, io.EOF) {
				break
			}
			return err
		}
	}
	tailHash, err := tailWindowHash(file, index.size)
	if err != nil {
		return err
	}
	index.tailHash = tailHash
	return nil
}
