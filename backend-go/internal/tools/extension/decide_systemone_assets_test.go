package extension

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

const (
	// assetCanary is valid base64 (of "CANARY"); no output may ever hold the image data.
	assetCanary = "Q0FOQVJZ"
	assetPNG    = "iVBORw0KGgo="
	assetOK     = `{"format":"png","data_base64":"` + assetCanary + `"}`
	assetsQ     = `[{"name":"q1","instructions":"Is u-1 a logo?"}]`
)

// assetsRig is a decide_systemone rig whose key reads are counted, with a roomy byte budget.
type assetsRig struct {
	*decideSystemoneRig
	tool     tools.Tool
	keyReads int
}

func newAssetsRig(t *testing.T, status int, reply string, adjust func(*sessionfs.ExtensionDecideSystemoneSettings)) *assetsRig {
	t.Helper()
	base := newDecideSystemoneRig(t, status, reply, func(s *sessionfs.ExtensionDecideSystemoneSettings) {
		s.MaxInputBytes = 1 << 20
		if adjust != nil {
			adjust(s)
		}
	})
	rig := &assetsRig{decideSystemoneRig: base}
	tool, err := newDecideSystemoneTool(base.settings, func(string) string { rig.keyReads++; return "k" }, base.server.Client())
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := tools.NewCatalog([]tools.Tool{tool})
	if err != nil {
		t.Fatal(err)
	}
	rig.tool, rig.catalog = tool, catalog
	return rig
}

// direct runs the tool's own code on the arguments after normalization, without the schema, so
// the parser's refusals are tested on their own.
func (r *assetsRig) direct(t *testing.T, arguments string) string {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal([]byte(arguments), &decoded); err != nil {
		t.Fatal(err)
	}
	output, err := r.tool.Execute(context.Background(), r.tool.NormalizeArguments(decoded), tools.ExecutionContext{CallID: "call_1"})
	if err != nil {
		t.Fatal(err)
	}
	return output
}

// refused asserts a local refusal: the code, the text, no request and no key read.
func (r *assetsRig) refused(t *testing.T, output, code, want string) {
	t.Helper()
	decideSystemoneRefused(t, output, len(r.requests), want)
	if !strings.Contains(output, "error_code: "+code) {
		t.Fatalf("want error_code %s, got:\n%s", code, output)
	}
	if r.keyReads != 0 {
		t.Fatalf("the key was read %d time(s) for a refused call", r.keyReads)
	}
}

func assetsCall(assets, questions string) string {
	if assets != "" {
		assets = `"assets":` + assets + `,`
	}
	return `{"input":"x",` + assets + `"questions":` + questions + `}`
}

func TestDecideSystemoneAssetsRequestBody(t *testing.T) {
	const (
		one   = `{"assets":{"u-1":{"type":"image","format":"png","data_base64":"` + assetPNG + `"}},"model":"m-1","questions":{"q1":{"instructions":"Is u-1 a logo?","type":"noul"}},"state":"x"}`
		plain = `{"model":"m-1","questions":{"q1":{"instructions":"Is u-1 a logo?","type":"noul"}},"state":"x"}`
	)
	for _, path := range decideSystemonePaths {
		for name, tc := range map[string]struct{ assets, questions, want string }{
			"one png":      {`{"u-1":{"format":"png","data_base64":"` + assetPNG + `"}}`, assetsQ, one},
			"type omitted": {`{"u-1":{"format":"png","data_base64":"` + assetPNG + `"}}`, assetsQ, one},
			"type image":   {`{"u-1":{"type":"image","format":"png","data_base64":"` + assetPNG + `"}}`, assetsQ, one},
			"png and jpg sorted by reference": {`{"b-2":{"format":"jpg","data_base64":"/9j/4AAQ"},"a-1":{"format":"png","data_base64":"iVBORw0K"}}`, assetsQ,
				`{"assets":{"a-1":{"type":"image","format":"png","data_base64":"iVBORw0K"},"b-2":{"type":"image","format":"jpg","data_base64":"/9j/4AAQ"}},"model":"m-1","questions":{"q1":{"instructions":"Is u-1 a logo?","type":"noul"}},"state":"x"}`},
			"jpeg is kept": {`{"u-1":{"format":"jpeg","data_base64":"/9j/4AAQ"}}`, assetsQ,
				`{"assets":{"u-1":{"type":"image","format":"jpeg","data_base64":"/9j/4AAQ"}},"model":"m-1","questions":{"q1":{"instructions":"Is u-1 a logo?","type":"noul"}},"state":"x"}`},
			"format spelling is normalized": {`{"a":{"format":" JPG ","data_base64":"/9j/4AAQ"},"b":{"format":"Jpeg","data_base64":"/9j/4AAQ"},"c":{"format":"PNG","data_base64":"iVBORw0K"}}`, `[{"name":"q1","instructions":"a b c"}]`,
				`{"assets":{"a":{"type":"image","format":"jpg","data_base64":"/9j/4AAQ"},"b":{"type":"image","format":"jpeg","data_base64":"/9j/4AAQ"},"c":{"type":"image","format":"png","data_base64":"iVBORw0K"}},"model":"m-1","questions":{"q1":{"instructions":"a b c","type":"noul"}},"state":"x"}`},
			"one asset for two questions": {`{"u-1":{"format":"png","data_base64":"` + assetPNG + `"}}`, `[{"name":"q1","instructions":"Is u-1 a logo?"},{"name":"q2","instructions":"Is u-1 red?"}]`,
				`{"assets":{"u-1":{"type":"image","format":"png","data_base64":"` + assetPNG + `"}},"model":"m-1","questions":{"q1":{"instructions":"Is u-1 a logo?","type":"noul"},"q2":{"instructions":"Is u-1 red?","type":"noul"}},"state":"x"}`},
			"assets null is absent":  {`null`, assetsQ, plain},
			"assets empty is absent": {`{}`, assetsQ, plain},
			"no assets":              {``, assetsQ, plain},
		} {
			t.Run(path+"/"+name, func(t *testing.T) {
				rig := newAssetsRig(t, 200, `{"answers":{}}`, func(s *sessionfs.ExtensionDecideSystemoneSettings) { s.Path = path })
				rig.call(t, assetsCall(tc.assets, tc.questions))
				if len(rig.requests) != 1 || string(rig.requests[0].Raw) != tc.want {
					t.Fatalf("body:\n%s\nwant:\n%s", rig.requests[0].Raw, tc.want)
				}
				wantImages := ""
				if strings.Contains(tc.want, `"assets"`) {
					wantImages = "true"
				}
				if rig.requests[0].Images != wantImages {
					t.Fatalf("images header %q, want %q", rig.requests[0].Images, wantImages)
				}
			})
		}
	}
}

// A reference is plain text for the provider: the tool sends it exactly where the caller wrote it.
func TestDecideSystemoneAssetsReferencesAreNeverReplaced(t *testing.T) {
	rig := newAssetsRig(t, 200, `{"answers":{}}`, nil)
	rig.call(t, `{"input":[{"type":"json","value":{"photo":"u-1"}},"see u-2 here"],"assets":{"u-1":`+assetOK+`,"u-2":`+assetOK+`},`+
		`"questions":[{"name":"a","instructions":"Is u-1 visible?","criteria":{"true":"u-2 is visible","false":"u-2 is not"}},{"name":"b","instructions":{"look":"u-1"}}]}`)
	if len(rig.requests) != 1 {
		t.Fatalf("%d requests", len(rig.requests))
	}
	raw := string(rig.requests[0].Raw)
	for _, want := range []string{
		`"state":[{"photo":"u-1"},"see u-2 here"]`,
		`"instructions":"Is u-1 visible?"`,
		`"criteria":{"true":"u-2 is visible","false":"u-2 is not"}`,
		`"instructions":{"look":"u-1"}`,
	} {
		if !strings.Contains(raw, want) {
			t.Fatalf("missing %s in:\n%s", want, raw)
		}
	}
	if strings.Count(raw, `"data_base64"`) != 2 {
		t.Fatalf("want both assets once:\n%s", raw)
	}
}

func TestDecideSystemoneImagesHeaderFollowsAssetsAndBox(t *testing.T) {
	box := []decideSystemoneQuestion{{Name: "a", Type: "predicate"}, {Name: "b", Type: "box"}}
	plain := []decideSystemoneQuestion{{Name: "a", Type: "predicate"}}
	assets := map[string]decideSystemoneAsset{"u": {Type: "image", Format: "png", DataBase64: "AAAA"}}
	for name, tc := range map[string]struct {
		questions []decideSystemoneQuestion
		assets    map[string]decideSystemoneAsset
		want      bool
	}{
		"neither":     {plain, nil, false},
		"empty":       {plain, map[string]decideSystemoneAsset{}, false},
		"box only":    {box, nil, true},
		"assets only": {plain, assets, true},
		"both":        {box, assets, true},
	} {
		if got := decideSystemoneNeedsImages(tc.questions, tc.assets); got != tc.want {
			t.Errorf("%s: %t, want %t", name, got, tc.want)
		}
	}
	// And on the wire: neither, assets only, both (a box alone is refused before any request).
	for _, path := range decideSystemonePaths {
		for _, longContext := range []bool{false, true} {
			for name, tc := range map[string]struct{ assets, questions, images string }{
				"neither":     {``, assetsQ, ""},
				"assets only": {`{"u-1":` + assetOK + `}`, assetsQ, "true"},
				"both":        {`{"u-1":` + assetOK + `}`, `[{"name":"q1","type":"box","instructions":"The u-1 logo."}]`, "true"},
			} {
				t.Run(fmt.Sprintf("%s/long=%t/%s", path, longContext, name), func(t *testing.T) {
					rig := newAssetsRig(t, 200, `{"answers":{}}`, func(s *sessionfs.ExtensionDecideSystemoneSettings) { s.Path = path; s.LongContext = longContext })
					rig.call(t, assetsCall(tc.assets, tc.questions))
					wantLong := ""
					if longContext {
						wantLong = "true"
					}
					if len(rig.requests) != 1 || rig.requests[0].Images != tc.images || rig.requests[0].LongContext != wantLong {
						t.Fatalf("requests %+v, want images %q long %q", rig.requests, tc.images, wantLong)
					}
				})
			}
		}
	}
}

func TestDecideSystemoneBoxNeedsAnImage(t *testing.T) {
	const boxOnly = `[{"name":"q1","type":"box","instructions":"The u-1 logo."}]`
	const want = "A box question needs an image"
	for name, assets := range map[string]string{"absent": ``, "null": `null`, "empty": `{}`} {
		t.Run(name, func(t *testing.T) {
			rig := newAssetsRig(t, 200, `{"answers":{}}`, nil)
			rig.refused(t, rig.call(t, assetsCall(assets, boxOnly)), "invalid_question", want)
		})
	}
	rig := newAssetsRig(t, 200, `{"answers":{"q1":{"type":"box","box":{"top":0.1,"bottom":0.9,"left":0.2,"right":0.8}}}}`, nil)
	output := rig.call(t, assetsCall(`{"u-1":`+assetOK+`}`, boxOnly))
	if len(rig.requests) != 1 || !strings.Contains(output, `"type":"box","box":{"top":0.1,"bottom":0.9,"left":0.2,"right":0.8}`) {
		t.Fatalf("a box with its image was not answered (%d requests):\n%s", len(rig.requests), output)
	}
}

// The order of refusals is fixed: input, assets, questions, box needs an image, question count, budget, image data.
func TestDecideSystemoneAssetsRefusalPrecedence(t *testing.T) {
	const badAssets = `{"u-1":{"format":"gif","data_base64":"` + assetCanary + `"}}`
	const badData = `{"u-1":{"format":"png","data_base64":"CANARY"}}`
	box := `{"name":"bx","type":"box","instructions":"The u-1 logo."}`
	for name, tc := range map[string]struct{ arguments, code, want string }{
		"input before assets":      {`{"input":"  ","assets":` + badAssets + `,"questions":` + assetsQ + `}`, "invalid_input", "the text is blank"},
		"assets before questions":  {`{"input":"x","assets":` + badAssets + `,"questions":[{"name":"a","type":"box"}]}`, "invalid_input", "format must be png"},
		"questions before box":     {`{"input":"x","questions":[{"name":"a","type":"box"}]}`, "invalid_question", "needs instructions that name the target"},
		"box before count":         {`{"input":"x","questions":[{"instructions":"1"},{"instructions":"2"},{"instructions":"3"},{"instructions":"4"},` + box + `]}`, "invalid_question", "A box question needs an image"},
		"count before budget":      {`{"input":"` + strings.Repeat("x", 300) + `","assets":{"u-1":` + assetOK + `},"questions":[{"instructions":"1 u-1"},{"instructions":"2"},{"instructions":"3"},{"instructions":"4"},{"instructions":"5"}]}`, "too_many_questions", "5 questions"},
		"budget before image data": {`{"input":"` + strings.Repeat("x", 300) + `","assets":` + badData + `,"questions":[{"instructions":"u-1"}]}`, "input_too_large", "max_input_bytes is 200"},
		"image data last":          {`{"input":"x","assets":` + badData + `,"questions":[{"instructions":"u-1"}]}`, "invalid_input", "standard base64"},
	} {
		t.Run(name, func(t *testing.T) {
			rig := newAssetsRig(t, 200, `{"answers":{}}`, func(s *sessionfs.ExtensionDecideSystemoneSettings) { s.MaxInputBytes = 200 })
			output := rig.direct(t, tc.arguments)
			rig.refused(t, output, tc.code, tc.want)
			if strings.Contains(output, "CANARY") || strings.Contains(output, assetCanary) {
				t.Fatalf("image data in the refusal:\n%s", output)
			}
		})
	}
}

func TestDecideSystemoneAssetsSchemaRefusals(t *testing.T) {
	for name, assets := range map[string]string{
		"assets is a string":  `"` + assetCanary + `"`,
		"assets is an array":  `[` + assetOK + `]`,
		"value is a string":   `{"u-1":"` + assetCanary + `"}`,
		"part":                `{"u-1":{"part":"p","format":"png","data_base64":"` + assetCanary + `"}}`,
		"url":                 `{"u-1":{"url":"https://example.com/a.png","format":"png","data_base64":"` + assetCanary + `"}}`,
		"path":                `{"u-1":{"path":"/etc/passwd","format":"png","data_base64":"` + assetCanary + `"}}`,
		"type is not image":   `{"u-1":{"type":"video","format":"png","data_base64":"` + assetCanary + `"}}`,
		"format is missing":   `{"u-1":{"data_base64":"` + assetCanary + `"}}`,
		"format is gif":       `{"u-1":{"format":"gif","data_base64":"` + assetCanary + `"}}`,
		"data is missing":     `{"u-1":{"format":"png"}}`,
		"data is a number":    `{"u-1":{"format":"png","data_base64":5}}`,
		"data is an object":   `{"u-1":{"format":"png","data_base64":{"a":"` + assetCanary + `"}}}`,
		"unknown extra field": `{"u-1":{"format":"png","data_base64":"` + assetCanary + `","extra":1}}`,
	} {
		t.Run(name, func(t *testing.T) {
			rig := newAssetsRig(t, 200, `{"answers":{}}`, nil)
			output := rig.call(t, assetsCall(assets, assetsQ))
			rig.refused(t, output, "validation_failed", "error_type: schema")
			if strings.Contains(output, assetCanary) {
				t.Fatalf("image data in the refusal:\n%s", output)
			}
		})
	}
}

// An empty object is no assets at all, so it can never switch the images header on.
func TestDecideSystemoneAssetsParserTreatsNullAndEmptyAsNone(t *testing.T) {
	for _, raw := range []any{nil, map[string]any{}} {
		assets, err := parseDecideSystemoneAssets(raw)
		if err != nil || assets != nil || decideSystemoneNeedsImages(nil, assets) {
			t.Fatalf("%v: assets %v, err %v", raw, assets, err)
		}
	}
}

// The parser refuses on its own what the schema also refuses, so a change to either cannot open a gap.
func TestDecideSystemoneAssetsParserRefusals(t *testing.T) {
	long := func(n int, s string) string { return strings.Repeat(s, n) }
	asset := func(extra string) string { return `{"format":"png","data_base64":"` + assetCanary + `"` + extra + `}` }
	for name, tc := range map[string]struct{ assets, want string }{
		"assets is a string":     {`"x"`, "assets must be an object"},
		"assets is an array":     {`[]`, "assets must be an object"},
		"value is a string":      {`{"u-1":"x"}`, "must be an object with format and data_base64"},
		"value is null":          {`{"u-1":null}`, "must be an object with format and data_base64"},
		"part":                   {`{"u-1":` + asset(`,"part":"p"`) + `}`, `"part" is not accepted`},
		"url":                    {`{"u-1":` + asset(`,"url":"u"`) + `}`, `"url" is not accepted`},
		"path":                   {`{"u-1":` + asset(`,"path":"p"`) + `}`, `"path" is not accepted`},
		"type is not image":      {`{"u-1":` + asset(`,"type":"video"`) + `}`, "type must be image"},
		"type is null":           {`{"u-1":` + asset(`,"type":null`) + `}`, "type must be image"},
		"type is a number":       {`{"u-1":` + asset(`,"type":1`) + `}`, "type must be image"},
		"format is missing":      {`{"u-1":{"data_base64":"` + assetCanary + `"}}`, "format must be png, jpg or jpeg"},
		"format is gif":          {`{"u-1":{"format":"gif","data_base64":"` + assetCanary + `"}}`, "format must be png, jpg or jpeg"},
		"format is a number":     {`{"u-1":{"format":1,"data_base64":"` + assetCanary + `"}}`, "format must be png, jpg or jpeg"},
		"data is missing":        {`{"u-1":{"format":"png"}}`, "data_base64 is missing or not a string"},
		"data is null":           {`{"u-1":{"format":"png","data_base64":null}}`, "data_base64 is missing or not a string"},
		"data is a number":       {`{"u-1":{"format":"png","data_base64":5}}`, "data_base64 is missing or not a string"},
		"reference is empty":     {`{"":` + assetOK + `}`, "empty or only whitespace"},
		"reference is spaces":    {`{"   ":` + assetOK + `}`, "empty or only whitespace"},
		"reference is a newline": {`{"\t\n":` + assetOK + `}`, "empty or only whitespace"},
		"reference 1025 bytes":   {`{"` + long(1025, "a") + `":` + assetOK + `}`, "the reference is 1025 bytes; the limit is 1024"},
		"reference 513 runes":    {`{"` + long(513, "é") + `":` + assetOK + `}`, "the reference is 1026 bytes; the limit is 1024"},
		"references 65537 bytes": {`{` + assetReferences(64, 1024, 1) + `}`, "together are over 65536 bytes"},
	} {
		t.Run(name, func(t *testing.T) {
			rig := newAssetsRig(t, 200, `{"answers":{}}`, nil)
			output := rig.direct(t, assetsCall(tc.assets, assetsQ))
			rig.refused(t, output, "invalid_input", tc.want)
			if strings.Contains(output, assetCanary) {
				t.Fatalf("image data in the refusal:\n%s", output)
			}
		})
	}
}

// assetReferences lists count assets whose references are size bytes each, then one more of one byte when extra is 1.
func assetReferences(count, size, extra int) string {
	var items []string
	for index := 0; index < count; index++ {
		reference := fmt.Sprintf("%04d", index) + strings.Repeat("r", size-4)
		items = append(items, `"`+reference+`":`+assetOK)
	}
	if extra > 0 {
		items = append(items, `"~":`+assetOK)
	}
	return strings.Join(items, ",")
}

func TestDecideSystemoneAssetsReferenceLimitsCountBytes(t *testing.T) {
	for name, assets := range map[string]string{
		"1024 ascii bytes":        `{"` + strings.Repeat("a", 1024) + `":` + assetOK + `}`,
		"1024 bytes of 512 runes": `{"` + strings.Repeat("é", 512) + `":` + assetOK + `}`,
		"65536 bytes in total":    `{` + assetReferences(64, 1024, 0) + `}`,
		"a single character":      `{" x ":` + assetOK + `}`,
	} {
		t.Run(name, func(t *testing.T) {
			rig := newAssetsRig(t, 200, `{"answers":{}}`, nil)
			output := rig.direct(t, assetsCall(assets, assetsQ))
			if len(rig.requests) != 1 || !strings.Contains(output, `"status":"ok"`) {
				t.Fatalf("a reference at the limit was refused (%d requests):\n%s", len(rig.requests), output)
			}
		})
	}
	// A long reference is quoted cut, never whole.
	rig := newAssetsRig(t, 200, `{"answers":{}}`, nil)
	output := rig.direct(t, assetsCall(`{"`+strings.Repeat("x", 1025)+`":`+assetOK+`}`, assetsQ))
	if !strings.Contains(output, strings.Repeat("x", 64)+`\"...`) || strings.Contains(output, strings.Repeat("x", 65)) {
		t.Fatalf("the reference is not cut to 64 bytes:\n%s", output)
	}
	// A multi-byte reference is cut on a character boundary.
	output = rig.direct(t, assetsCall(`{"`+strings.Repeat("é", 600)+`":`+assetOK+`}`, assetsQ))
	if !strings.Contains(output, strings.Repeat("é", 32)+`\"...`) || strings.Contains(output, strings.Repeat("é", 33)) {
		t.Fatalf("the multi-byte reference is not cut to 64 bytes:\n%s", output)
	}
}

func TestDecideSystemoneAssetsBase64Rule(t *testing.T) {
	for name, data := range map[string]string{
		"empty":                   ``,
		"length not a multiple":   `Q0FOQVJ`,
		"leading space":           ` Q0FOQVJZ`,
		"trailing newline":        "Q0FOQVJZ\n",
		"inner space":             `Q0F QVJZ`,
		"inner newline":           "Q0F\nQVJZ",
		"inner carriage return":   "Q0F\rQVJZ",
		"inner tab":               "Q0F\tQVJZ",
		"space in the last group": `Q0FOQV Z`,
		"leading newline":         "\nQ0FOQVJ",
		"trailing space":          `Q0FOQVJ `,
		"data prefix":             `data:image/png;base64,Q0FOQVJZ`,
		"url safe minus":          `Q0FO-VJZ`,
		"url safe underscore":     `Q0FO_VJZ`,
		"padding in the middle":   `Q0=OQVJZ`,
		"padding before data":     `=0FOQVJZ`,
		"three padding":           `Q===`,
		"only padding":            `====`,
		"padding then data":       `Q0FOQ=JZ`,
		"unpadded":                `Q0FOQQ`,
		"non ascii":               `Q0FOQVJé`,
		"quote":                   `Q0FOQV"Z`,
		"too much padding":        `Q0FOQ===`,
	} {
		t.Run(name, func(t *testing.T) {
			rig := newAssetsRig(t, 200, `{"answers":{}}`, nil)
			encoded, _ := json.Marshal(data)
			output := rig.direct(t, assetsCall(`{"u-1":{"format":"png","data_base64":`+string(encoded)+`}}`, assetsQ))
			rig.refused(t, output, "invalid_input", "data_base64 must be standard base64")
			if data != "" && strings.Contains(output, strings.TrimSpace(data)) && len(strings.TrimSpace(data)) > 3 {
				t.Fatalf("image data in the refusal:\n%s", output)
			}
		})
	}
	for _, data := range []string{`Q0FOQVJZ`, `Q0FOQUE=`, `Q0FOQQ==`, `AAAA`, `AA==`, `+/+/`, `abcdXYZ0`, `Zg==`} {
		t.Run("valid "+data, func(t *testing.T) {
			rig := newAssetsRig(t, 200, `{"answers":{}}`, nil)
			rig.call(t, assetsCall(`{"u-1":{"format":"png","data_base64":"`+data+`"}}`, assetsQ))
			if len(rig.requests) != 1 || !strings.Contains(string(rig.requests[0].Raw), `"data_base64":"`+data+`"`) {
				t.Fatalf("valid base64 %q was refused or altered: %+v", data, rig.requests)
			}
		})
	}
	// Whitespace around the data is never trimmed away.
	rig := newAssetsRig(t, 200, `{"answers":{}}`, nil)
	output := rig.direct(t, assetsCall(`{"u-1":{"format":"png","data_base64":" Q0FOQVJZ "}}`, assetsQ))
	rig.refused(t, output, "invalid_input", "standard base64")
}

func TestDecideSystemoneAssetsAreCountedInTheByteBudget(t *testing.T) {
	const (
		input     = "x"
		questions = `{"q1":{"instructions":"Is u-1 a logo?","type":"noul"}}`
		assets    = `{"u-1":{"type":"image","format":"png","data_base64":"` + assetCanary + `"}}`
	)
	exact := int64(len(input) + len(questions) + len(assets))
	args := assetsCall(`{"u-1":`+assetOK+`}`, assetsQ)
	for _, limit := range []int64{exact, exact + 1} {
		rig := newAssetsRig(t, 200, `{"answers":{}}`, func(s *sessionfs.ExtensionDecideSystemoneSettings) { s.MaxInputBytes = limit })
		if output := rig.call(t, args); len(rig.requests) != 1 || !strings.Contains(output, `"status":"ok"`) {
			t.Fatalf("limit %d: a request exactly at the limit was refused:\n%s", limit, output)
		}
	}
	rig := newAssetsRig(t, 200, `{"answers":{}}`, func(s *sessionfs.ExtensionDecideSystemoneSettings) { s.MaxInputBytes = exact - 1 })
	want := fmt.Sprintf("The request is %d bytes (%d input, %d questions, %d assets); max_input_bytes is %d", exact, len(input), len(questions), len(assets), exact-1)
	rig.refused(t, rig.call(t, args), "input_too_large", want)
	// The legacy message is unchanged when no image is sent.
	rig = newAssetsRig(t, 200, `{"answers":{}}`, func(s *sessionfs.ExtensionDecideSystemoneSettings) { s.MaxInputBytes = 10 })
	rig.refused(t, rig.call(t, assetsCall("", assetsQ)), "input_too_large", fmt.Sprintf("The request is %d bytes (1 input, %d questions); max_input_bytes is 10", 1+len(questions), len(questions)))
}

// The budget's count of the assets is the size of the assets object on the wire, byte for byte.
func TestDecideSystemoneAssetsSizeEqualsTheMarshaledSize(t *testing.T) {
	check := func(label string, assets map[string]decideSystemoneAsset) {
		t.Helper()
		encoded, err := tools.MarshalJSON(assets)
		if err != nil {
			t.Fatal(err)
		}
		if got := decideSystemoneAssetsSize(assets); got != len(encoded) {
			t.Errorf("%s: counted %d bytes, marshaled %d: %s", label, got, len(encoded), encoded)
		}
	}
	data := map[string]string{"png": "iVBORw0KGgo=", "jpg": "/9j/4AAQ", "jpeg": "AA=="}
	build := func(references ...string) map[string]decideSystemoneAsset {
		assets := map[string]decideSystemoneAsset{}
		for index, reference := range references {
			format := []string{"png", "jpg", "jpeg"}[index%3]
			assets[reference] = decideSystemoneAsset{Type: "image", Format: format, DataBase64: data[format]}
		}
		return assets
	}
	if decideSystemoneAssetsSize(nil) != 0 || decideSystemoneAssetsSize(map[string]decideSystemoneAsset{}) != 0 {
		t.Error("no assets must count as no bytes, since none are sent")
	}
	check("one", build("u-1"))
	check("three", build("a", "b", "c"))
	check("shapes", build(`q"uote`, `<tag>`, `a&b`, `back\slash`, "tab\tnl\n", "\x00\x01\x1f\x7f", "e\u0301é日本語😀", "sep\u2028\u2029x", strings.Repeat("k", 1024), "'single'"))
	// Every code point of the first planes of control, ASCII, Latin and the separators.
	for r := rune(0); r < 0x3000; r++ {
		check(fmt.Sprintf("U+%04X", r), build("k"+string(r)+"k"))
	}
	check("invalid UTF-8 key", build(string([]byte{0x6b, 0xff, 0x6b})))
	check("big data", map[string]decideSystemoneAsset{"u": {Type: "image", Format: "jpg", DataBase64: strings.Repeat("QUJD", 1<<16)}})
}

// The size on the wire is also what a real request carries, special reference included.
func TestDecideSystemoneAssetsSizeMatchesTheRequestBody(t *testing.T) {
	rig := newAssetsRig(t, 200, `{"answers":{}}`, nil)
	const reference = "u\"<&>\u2028é日-1"
	encoded, _ := json.Marshal(reference)
	rig.call(t, `{"input":"x","assets":{`+string(encoded)+`:`+assetOK+`},"questions":[{"name":"q1","instructions":"Is `+strings.Trim(string(encoded), `"`)+` a logo?"}]}`)
	if len(rig.requests) != 1 {
		t.Fatalf("%d requests", len(rig.requests))
	}
	var body struct {
		Assets json.RawMessage `json:"assets"`
	}
	if err := json.Unmarshal(rig.requests[0].Raw, &body); err != nil {
		t.Fatal(err)
	}
	assets := map[string]decideSystemoneAsset{reference: {Type: "image", Format: "png", DataBase64: assetCanary}}
	if got := decideSystemoneAssetsSize(assets); got != len(body.Assets) {
		t.Fatalf("counted %d bytes, sent %d: %s", got, len(body.Assets), body.Assets)
	}
}

func TestDecideSystemoneAssetsProviderErrorsNeverCarryTheImage(t *testing.T) {
	const data = "Q0FOQVJZQ0FOQVJZQ0FOQVJZ"
	args := assetsCall(`{"u-1":{"format":"png","data_base64":"`+data+`"}}`, assetsQ)
	for name, tc := range map[string]struct {
		status      int
		reply, code string
	}{
		"unreferenced asset": {400, `{"error":{"message":"Asset 'u-1' is not referenced anywhere in the request","code":400}}`, "request_rejected"},
		"bad image":          {400, `{"error":{"message":"Image rejected: the file is not a PNG or JPEG, or is damaged","code":400}}`, "request_rejected"},
		"too large":          {413, `{"error":{"message":"Request body too large","code":413}}`, "request_rejected"},
		"detail":             {422, `{"detail":[{"loc":["body","assets","u-1","data_base64"],"msg":"Invalid base64","type":"value_error"}]}`, "request_rejected"},
		"not json":           {413, `<html>413 Request Entity Too Large</html>`, "request_rejected"},
	} {
		t.Run(name, func(t *testing.T) {
			rig := newAssetsRig(t, tc.status, tc.reply, nil)
			output := rig.call(t, args)
			if !strings.Contains(output, `"status":"invalid_input"`) || !strings.Contains(output, "error_code: "+tc.code) || !strings.Contains(output, fmt.Sprintf("HTTP %d", tc.status)) {
				t.Fatalf("want %s for HTTP %d, got:\n%s", tc.code, tc.status, output)
			}
			if strings.Contains(output, data) || strings.Contains(output, "QUJD") {
				t.Fatalf("image data in the output:\n%s", output)
			}
		})
	}
	// A call that works returns answers only.
	rig := newAssetsRig(t, 200, decideSystemoneOKReply(), nil)
	if output := rig.call(t, args); !strings.Contains(output, `"status":"ok"`) || strings.Contains(output, data) {
		t.Fatalf("output:\n%s", output)
	}
}

// Only the format is touched when arguments are normalized, and never the image string.
func TestDecideSystemoneAssetsNormalizationTouchesOnlyTheFormat(t *testing.T) {
	data := strings.Repeat("QUJD", 1<<18)
	built := func(format string) map[string]any {
		return map[string]any{"u": map[string]any{"format": format, "data_base64": data}, "v": map[string]any{"format": "png", "data_base64": data}}
	}
	same := built("png")
	if got := normalizeDecideSystemoneAssets(same); reflect.ValueOf(got).Pointer() != reflect.ValueOf(same).Pointer() {
		t.Fatal("assets that need no change were copied")
	}
	changed := built(" JPG\n")
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	fixed, _ := normalizeDecideSystemoneAssets(changed).(map[string]any)
	runtime.ReadMemStats(&after)
	if got := fixed["u"].(map[string]any); got["format"] != "jpg" || got["data_base64"] != data {
		t.Fatalf("normalized %v", got["format"])
	}
	if changed["u"].(map[string]any)["format"] != " JPG\n" {
		t.Fatal("the caller's arguments were modified")
	}
	if delta := after.TotalAlloc - before.TotalAlloc; delta > 16<<10 {
		t.Fatalf("normalization allocated %d bytes for %d bytes of image data", delta, 2*len(data))
	}
	for _, value := range []any{nil, map[string]any{}} {
		if normalizeDecideSystemoneAssets(value) != nil {
			t.Fatalf("%v is not the same as no assets", value)
		}
	}
	if !reflect.DeepEqual(normalizeDecideSystemoneAssets("x"), "x") {
		t.Fatal("a value that is not an object must be left to the schema")
	}
}

// ---- Memory ----

func assetsPayload(size int) string { return strings.Repeat("QUJD", size/4) }

// assetsPipeline is the tool's local work for a call with one image: parse, count, scan, encode the request.
func assetsPipeline(tb testing.TB, arguments map[string]any, settings sessionfs.ExtensionDecideSystemoneSettings) int {
	tb.Helper()
	assets, err := parseDecideSystemoneAssets(arguments["assets"])
	if err != nil {
		tb.Fatal(err)
	}
	input, err := parseDecideSystemoneInput(arguments["input"])
	if err != nil {
		tb.Fatal(err)
	}
	questions, err := parseDecideSystemoneQuestions(arguments["questions"], nil)
	if err != nil {
		tb.Fatal(err)
	}
	questionBytes, err := tools.MarshalJSON(systemoneQuestionItems(questions))
	if err != nil {
		tb.Fatal(err)
	}
	if int64(len(questionBytes)+decideSystemoneAssetsSize(assets)) > settings.MaxInputBytes {
		tb.Fatal("over budget")
	}
	if err := checkDecideSystemoneAssetData(assets); err != nil {
		tb.Fatal(err)
	}
	payload, err := tools.MarshalJSON(systemoneRequest(settings.Model, input, questions, assets))
	if err != nil {
		tb.Fatal(err)
	}
	return len(payload)
}

func assetsArguments(size int) map[string]any {
	return map[string]any{
		"input":     []any{"x"},
		"assets":    map[string]any{"u-1": map[string]any{"format": "png", "data_base64": assetsPayload(size)}},
		"questions": []any{map[string]any{"name": "q1", "instructions": "Is u-1 a logo?"}},
	}
}

// Checking and counting an image costs O(number of assets), not O(image): nothing is decoded or copied.
func TestDecideSystemoneAssetsValidationAllocatesNothingPerByte(t *testing.T) {
	const size = 4 << 20
	arguments := assetsArguments(size)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	assets, err := parseDecideSystemoneAssets(arguments["assets"])
	counted := decideSystemoneAssetsSize(assets)
	scanned := checkDecideSystemoneAssetData(assets)
	runtime.ReadMemStats(&after)
	if err != nil || scanned != nil || counted < size {
		t.Fatalf("parse %v, scan %v, counted %d", err, scanned, counted)
	}
	if delta := after.TotalAlloc - before.TotalAlloc; delta > 256<<10 {
		t.Fatalf("validating and counting %d bytes of base64 allocated %d bytes", size, delta)
	}
}

// The allocation of the whole local pipeline is reported, not gated: the shared marshaler holds the
// request in a few buffers, which the log shows as a multiple of the image.
func TestDecideSystemoneAssetsPipelineAllocationReport(t *testing.T) {
	settings := sessionfs.ExtensionDecideSystemoneSettings{Model: "m-1", MaxInputBytes: 1 << 30}
	for _, size := range []int{64 << 10, 1 << 20, 4 << 20} {
		arguments := assetsArguments(size)
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		sent := assetsPipeline(t, arguments, settings)
		runtime.ReadMemStats(&after)
		delta := after.TotalAlloc - before.TotalAlloc
		t.Logf("base64 %8d bytes: request %8d bytes, allocated %9d bytes = %.2fx the image, %d allocations", size, sent, delta, float64(delta)/float64(size), after.Mallocs-before.Mallocs)
	}
}

func BenchmarkDecideSystemoneAssets(b *testing.B) {
	settings := sessionfs.ExtensionDecideSystemoneSettings{Model: "m-1", MaxInputBytes: 1 << 30}
	for _, size := range []int{64 << 10, 1 << 20, 4 << 20} {
		arguments := assetsArguments(size)
		b.Run(fmt.Sprintf("%dKiB", size>>10), func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(size))
			for i := 0; i < b.N; i++ {
				assetsPipeline(b, arguments, settings)
			}
		})
	}
	b.Run("validate 4MiB", func(b *testing.B) {
		assets, _ := parseDecideSystemoneAssets(assetsArguments(4 << 20)["assets"])
		b.ReportAllocs()
		b.SetBytes(4 << 20)
		for i := 0; i < b.N; i++ {
			decideSystemoneAssetsSize(assets)
			if err := checkDecideSystemoneAssetData(assets); err != nil {
				b.Fatal(err)
			}
		}
	})
}
