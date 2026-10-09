package webui

import (
	"slices"
	"strings"

	"example.com/agent-harness/backend-go/internal/imagestore"
)

// imagePlaceholder stands in for the text of a user message that has images and no text,
// so the message is still listed. An inline image is never sent to the browser.
const imagePlaceholder = "[image]"

// UserContentPart is one part of a persisted user message, reduced to what the Web UI reads.
type UserContentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	ImageURL string `json:"image_url"`
}

// UserContent reads the visible text of a user message and the stored image files it
// refers to. text joins the input_text parts. An image still inline as a data URL is
// not listed, but it makes the message visible: with no text, text is one placeholder per
// image. visible is false when the message has neither text nor an image.
func UserContent(content []UserContentPart) (text string, images []string, visible bool) {
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
		}
	}
	text = builder.String()
	if text == "" && imageCount > 0 {
		text = strings.TrimSuffix(strings.Repeat(imagePlaceholder+" ", imageCount), " ")
	}
	return text, images, text != ""
}
