package webui

import (
	"slices"
	"strings"

	"example.com/agent-harness/backend-go/internal/imagestore"
)

// imagePlaceholder stands in for the text of a user message that has images and no text,
// so the message is still listed. An inline image is never sent to the browser.
const imagePlaceholder = "[image]"

// defaultFileName names a file part that carries no filename.
const defaultFileName = "file"

// UserContentPart is one part of a persisted user message, reduced to what the Web UI reads.
type UserContentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	ImageURL string `json:"image_url"`
	Filename string `json:"filename"`
}

// UserContent reads the visible text of a user message, the stored image files it refers
// to and the names of the files (input_file) attached to it. text joins the input_text
// parts. An image still inline as a data URL is not listed, but it makes the message
// visible: with no text, text is one placeholder per image, then one "[file: name]" per
// file. The file bytes are never sent to the browser, only the names, in order and
// repeats kept. visible is false when the message has no text, image or file.
func UserContent(content []UserContentPart) (text string, images, files []string, visible bool) {
	var builder strings.Builder
	imageCount := 0
	for _, part := range content {
		switch part.Type {
		case "input_text":
			builder.WriteString(part.Text)
		case "input_image":
			imageCount++
			name, ok := strings.CutPrefix(part.ImageURL, imagestore.RefPrefix)
			if _, valid := imagestore.ValidName(name); ok && valid && !slices.Contains(images, name) {
				images = append(images, name)
			}
		case "input_file":
			name := strings.TrimSpace(part.Filename)
			if name == "" {
				name = defaultFileName
			}
			files = append(files, name)
		}
	}
	text = builder.String()
	if text == "" && (imageCount > 0 || len(files) > 0) {
		placeholders := make([]string, 0, imageCount+len(files))
		for range imageCount {
			placeholders = append(placeholders, imagePlaceholder)
		}
		for _, name := range files {
			placeholders = append(placeholders, "[file: "+name+"]")
		}
		text = strings.Join(placeholders, " ")
	}
	return text, images, files, text != ""
}
