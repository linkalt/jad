package httpapi

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"

	"example.com/agent-harness/backend-go/internal/imagedata"
)

// maxFilenameRunes bounds the file name kept in the transcript and shown in the Web UI.
const maxFilenameRunes = 255

// pdfMagic starts every PDF file.
var pdfMagic = []byte("%PDF-")

// checkedFileData decodes one file of a run request to check it and returns it as the
// base64 data URL a Responses input_file takes. Only PDF is accepted, judged by the first
// bytes and never by the name or a declared type.
func checkedFileData(label, data string, maxBytes int64) (string, error) {
	decoded, err := imagedata.Decode(data, maxBytes)
	switch err {
	case nil:
	case imagedata.ErrTooLarge:
		return "", &StatusError{Code: 413, Message: fmt.Sprintf("%s: the file is larger than %d bytes", label, maxBytes)}
	default:
		return "", badInput(label + ": " + err.Error())
	}
	if !bytes.HasPrefix(decoded, pdfMagic) {
		return "", badInput(label + ": the file is not a PDF; only PDF files are supported")
	}
	return "data:application/pdf;base64," + base64.StdEncoding.EncodeToString(decoded), nil
}

// cleanFilename reduces a client-supplied name to something safe to store and show: the
// last path element, no control characters, at most maxFilenameRunes runes. An empty
// result is refused, because the provider and the Web UI both need a name.
func cleanFilename(label, name string) (string, error) {
	name = strings.ReplaceAll(name, `\`, "/")
	name = path.Base(strings.TrimSpace(name))
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == "/" {
		return "", badInput(label + " needs a file name")
	}
	if utf8.RuneCountInString(name) > maxFilenameRunes {
		runes := []rune(name)
		name = string(runes[:maxFilenameRunes])
	}
	return name, nil
}
