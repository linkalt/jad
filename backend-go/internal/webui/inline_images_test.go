package webui

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

var pngBytes = append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{7}, 24)...)

func dataURL(mime string, data []byte) string {
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)
}

func outputRecord(output string) json.RawMessage {
	return json.RawMessage(`{"type":"function_call_output","call_id":"c1","output":` + output + `}`)
}

func TestToolResultCountsInlineImages(t *testing.T) {
	png := dataURL("image/png", pngBytes)
	stored := "stored-image://" + strings.Repeat("a", 64) + ".png"
	tests := []struct {
		name   string
		output string
		want   int
	}{
		{"one inline png", `[{"type":"input_image","image_url":"` + png + `"}]`, 1},
		{"two inline, text between", `[{"type":"input_image","image_url":"` + png + `"},{"type":"input_text","text":"x"},{"type":"input_image","image_url":"` + dataURL("image/gif", []byte("GIF89a")) + `"}]`, 2},
		{"stored reference is not inline", `[{"type":"input_image","image_url":"` + stored + `"}]`, 0},
		{"http url is not inline", `[{"type":"input_image","image_url":"https://example.com/a.png"}]`, 0},
		{"svg is not shown", `[{"type":"input_image","image_url":"data:image/svg+xml;base64,PHN2Zz4="}]`, 0},
		{"data url without base64", `[{"type":"input_image","image_url":"data:image/png,abc"}]`, 0},
		{"data url on a non-image part", `[{"type":"input_text","image_url":"` + png + `"}]`, 0},
		{"plain string output", `"text"`, 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			part, ok := ToolResultPart(outputRecord(test.output), 0)
			if !ok {
				t.Fatal("not a tool result")
			}
			if part.InlineImages != test.want {
				t.Fatalf("InlineImages = %d, want %d", part.InlineImages, test.want)
			}
		})
	}
}

func TestInlineImagesAreCountedButTheBytesNeverReachTheMessage(t *testing.T) {
	record := outputRecord(`[{"type":"input_text","text":"shot"},{"type":"input_image","image_url":"` + dataURL("image/png", pngBytes) + `"}]`)
	part, _ := ToolResultPart(record, 0)
	encoded, _ := json.Marshal(part)
	if !strings.Contains(string(encoded), `"inlineImages":1`) || strings.Contains(string(encoded), "base64") {
		t.Fatalf("json = %s", encoded)
	}
	plain, _ := json.Marshal(Part{Type: "tool_result", CallID: "x"})
	if strings.Contains(string(plain), "inlineImages") {
		t.Fatalf("zero InlineImages must be omitted: %s", plain)
	}
}

func TestInlineImageDecodesTheIndexedImage(t *testing.T) {
	gif := append([]byte("GIF89a"), 1, 2, 3)
	record := outputRecord(`[{"type":"input_image","image_url":"` + dataURL("image/png", pngBytes) + `"},{"type":"input_image","image_url":"stored-image://` + strings.Repeat("a", 64) + `.png"},{"type":"input_image","image_url":"` + dataURL("image/gif", gif) + `"}]`)
	data, contentType, ok := InlineImage(record, 0)
	if !ok || contentType != "image/png" || !bytes.Equal(data, pngBytes) {
		t.Fatalf("index 0 = %q %q %v", data, contentType, ok)
	}
	data, contentType, ok = InlineImage(record, 1)
	if !ok || contentType != "image/gif" || !bytes.Equal(data, gif) {
		t.Fatalf("index 1 (the stored reference is skipped) = %q %q %v", data, contentType, ok)
	}
	for _, index := range []int{-1, 2, 99} {
		if _, _, ok := InlineImage(record, index); ok {
			t.Fatalf("index %d must not resolve", index)
		}
	}
}

func TestInlineImageRefusesWhatItCannotVouchFor(t *testing.T) {
	tests := []struct {
		name   string
		record json.RawMessage
	}{
		{"bytes are not the claimed type", outputRecord(`[{"type":"input_image","image_url":"` + dataURL("image/png", []byte("<html><script>alert(1)</script>")) + `"}]`)},
		{"payload is not base64", outputRecord(`[{"type":"input_image","image_url":"data:image/png;base64,@@@"}]`)},
		{"claimed jpeg, real png", outputRecord(`[{"type":"input_image","image_url":"` + dataURL("image/jpeg", pngBytes) + `"}]`)},
		{"not a tool output record", json.RawMessage(`{"type":"message","role":"user","output":[{"type":"input_image","image_url":"` + dataURL("image/png", pngBytes) + `"}]}`)},
		{"not json", json.RawMessage(`nope`)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, _, ok := InlineImage(test.record, 0); ok {
				t.Fatal("must be refused")
			}
		})
	}
}

func messageWithInlineImage(cursor *int64) Message {
	return Message{ID: "m1", Cursor: cursor, Parts: []Part{
		{Type: "tool_call", CallID: "c1", Name: "shot"},
		{Type: "tool_result", CallID: "c1", Output: "[MCP image content]", InlineImages: 1},
	}}
}

func TestCapMessagesKeepsTheCursorOfAMessageWithInlineImages(t *testing.T) {
	cursor := int64(120)
	for _, limit := range []int{16 * 1024, 0} {
		capped := CapMessages([]Message{messageWithInlineImage(&cursor)}, limit, nil)
		if capped[0].Cursor == nil || *capped[0].Cursor != cursor {
			t.Fatalf("limit %d: Cursor = %v, want %d", limit, capped[0].Cursor, cursor)
		}
	}
}

func TestCapMessagesStillDropsTheCursorOfAnOrdinaryMessage(t *testing.T) {
	cursor := int64(120)
	plain := Message{ID: "m2", Cursor: &cursor, Parts: []Part{{Type: "text", Text: "hi"}}}
	for _, limit := range []int{16 * 1024, 0} {
		if capped := CapMessages([]Message{plain}, limit, nil); capped[0].Cursor != nil {
			t.Fatalf("limit %d: Cursor = %d, want none", limit, *capped[0].Cursor)
		}
	}
	if capped := CapMessages([]Message{messageWithInlineImage(nil)}, 16*1024, nil); capped[0].Cursor != nil {
		t.Fatal("a live message has no cursor to keep")
	}
}

func turnRecords(prefix string, outputs ...string) []TranscriptRecord {
	records := []TranscriptRecord{}
	offset := int64(0)
	for turn, output := range outputs {
		records = append(records,
			TranscriptRecord{Offset: offset, Prefix: prefix, Raw: json.RawMessage(`{"type":"message","role":"user","content":[{"type":"input_text","text":"go ` + string(rune('a'+turn)) + `"}]}`)},
			TranscriptRecord{Offset: offset + 1, Prefix: prefix, Raw: json.RawMessage(output)},
		)
		offset += 2
	}
	return records
}

func toolOutputWithPNG(callID string, body []byte) string {
	call := ""
	if callID != "" {
		call = `"call_id":"` + callID + `",`
	}
	return `{"type":"function_call_output",` + call + `"output":[{"type":"input_image","image_url":"` + dataURL("image/png", body) + `"}]}`
}

func TestFindInlineImageServesTheImageOfTheNamedMessageWhenACallIDRepeats(t *testing.T) {
	first := append([]byte("\x89PNG\r\n\x1a\n"), 1, 1, 1, 1)
	second := append([]byte("\x89PNG\r\n\x1a\n"), 2, 2, 2, 2)
	records := turnRecords("b", toolOutputWithPNG("call_0", first), toolOutputWithPNG("call_0", second))
	messages, err := NormalizeTranscript(records)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, message := range messages {
		for _, part := range message.Parts {
			if part.Type == "tool_result" && part.InlineImages > 0 {
				ids = append(ids, message.ID)
			}
		}
	}
	if len(ids) != 2 || ids[0] == ids[1] {
		t.Fatalf("messages with an inline image = %v, want two different ones", ids)
	}
	for position, want := range [][]byte{first, second} {
		data, _, ok := FindInlineImage(records, ids[position], "call_0", 0)
		if !ok || !bytes.Equal(data, want) {
			t.Fatalf("message %s served %v (ok %v), want %v", ids[position], data, ok, want)
		}
	}
}

func TestFindInlineImageServesAnOutputThatHadNoCallID(t *testing.T) {
	records := turnRecords("o", toolOutputWithPNG("", pngBytes))
	messages, err := NormalizeTranscript(records)
	if err != nil {
		t.Fatal(err)
	}
	var message Message
	var part Part
	for _, candidate := range messages {
		for _, p := range candidate.Parts {
			if p.Type == "tool_result" && p.InlineImages > 0 {
				message, part = candidate, p
			}
		}
	}
	if part.CallID == "" {
		t.Fatal("the part must carry the synthetic call id")
	}
	if data, _, ok := FindInlineImage(records, message.ID, part.CallID, 0); !ok || !bytes.Equal(data, pngBytes) {
		t.Fatalf("served %v (ok %v)", data, ok)
	}
}

func TestFindInlineImageRefusesAnotherMessageAnotherCallAndAnotherPosition(t *testing.T) {
	records := turnRecords("o", toolOutputWithPNG("c1", pngBytes))
	messages, _ := NormalizeTranscript(records)
	id := messages[len(messages)-1].ID
	for name, got := range map[string]bool{
		"unknown message":  func() bool { _, _, ok := FindInlineImage(records, "nope", "c1", 0); return ok }(),
		"unknown call":     func() bool { _, _, ok := FindInlineImage(records, id, "c2", 0); return ok }(),
		"position past it": func() bool { _, _, ok := FindInlineImage(records, id, "c1", 1); return ok }(),
		"no records":       func() bool { _, _, ok := FindInlineImage(nil, id, "c1", 0); return ok }(),
	} {
		if got {
			t.Errorf("%s must not resolve", name)
		}
	}
}

func TestFindInlineImageServesAResultThatJoinedItsToolCallMessage(t *testing.T) {
	records := []TranscriptRecord{
		{Offset: 0, Prefix: "o", Raw: json.RawMessage(`{"type":"message","role":"user","content":[{"type":"input_text","text":"shoot"}]}`)},
		{Offset: 1, Prefix: "o", Raw: json.RawMessage(`{"type":"response.completed","response":{"id":"resp-1","output":[{"type":"function_call","call_id":"call_9","name":"shot","arguments":"{}"}]}}`)},
		{Offset: 2, Prefix: "o", Raw: json.RawMessage(toolOutputWithPNG("call_9", pngBytes))},
	}
	messages, err := NormalizeTranscript(records)
	if err != nil {
		t.Fatal(err)
	}
	var joined *Message
	for i := range messages {
		for _, part := range messages[i].Parts {
			if part.Type == "tool_result" && part.InlineImages == 1 {
				joined = &messages[i]
			}
		}
	}
	if joined == nil || joined.ID != "resp-1" {
		t.Fatalf("the result must join the response message resp-1, got %+v", joined)
	}
	if data, _, ok := FindInlineImage(records, "resp-1", "call_9", 0); !ok || !bytes.Equal(data, pngBytes) {
		t.Fatalf("served %v (ok %v)", data, ok)
	}
}

// TestProjectingOneCallGroupsMessagesAsAFullProjection: FindInlineImage reads the output
// of the asked call only. The others still join their messages, so every message keeps
// its id, its steps and its parts in place, and the asked call's part is whole.
func TestProjectingOneCallGroupsMessagesAsAFullProjection(t *testing.T) {
	records := historyFixture(4, 64, true)
	records = append(records,
		TranscriptRecord{Offset: 900001, Prefix: "o", Raw: json.RawMessage(`{"type":"function_call_output","output":"orphan without call id"}`)},
		TranscriptRecord{Offset: 900002, Prefix: "o", Raw: json.RawMessage(`{"type":"function_call_output","call_id":"call-1","output":"a repeated call id"}`)},
	)
	full, err := NormalizeTranscript(records)
	if err != nil {
		t.Fatal(err)
	}
	for _, asked := range []string{"call-0", "call-2", "tool-call-o900001", "none"} {
		projected, err := normalizeTranscript(records, nil, func(id string) bool { return id == asked })
		if err != nil {
			t.Fatal(err)
		}
		if len(projected) != len(full) {
			t.Fatalf("%s: %d messages, want %d", asked, len(projected), len(full))
		}
		for index := range full {
			want, got := full[index], projected[index]
			if got.ID != want.ID || !reflect.DeepEqual(got.Steps, want.Steps) || len(got.Parts) != len(want.Parts) {
				t.Fatalf("%s: message %d = %s %+v (%d parts), want %s %+v (%d parts)", asked, index, got.ID, got.Steps, len(got.Parts), want.ID, want.Steps, len(want.Parts))
			}
			for position := range want.Parts {
				wantPart, gotPart := want.Parts[position], got.Parts[position]
				if gotPart.Type != wantPart.Type || gotPart.CallID != wantPart.CallID {
					t.Fatalf("%s: message %d part %d = %s %s, want %s %s", asked, index, position, gotPart.Type, gotPart.CallID, wantPart.Type, wantPart.CallID)
				}
				if wantPart.Type == "tool_result" && wantPart.CallID == asked && !reflect.DeepEqual(gotPart, wantPart) {
					t.Fatalf("%s: asked part = %+v, want %+v", asked, gotPart, wantPart)
				}
			}
		}
	}
}
