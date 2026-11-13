package extension

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

const boxQuestion = `{"name":"q1","type":"box","instructions":"The logo."}`

// boxAssets is the image a box question needs: any well-formed base64 will do for the fake provider.
const boxAssets = `"assets":{"img-1":{"format":"png","data_base64":"AAAA"}},`

// decideSystemoneContentText returns the answers line of an ok result exactly as rendered.
func decideSystemoneContentText(t *testing.T, output string) string {
	t.Helper()
	index := strings.Index(output, `{"answers"`)
	if index < 0 {
		t.Fatalf("no answers in:\n%s", output)
	}
	text := output[index:]
	if end := strings.Index(text, "\n"); end >= 0 {
		text = text[:end]
	}
	return text
}

func TestDecideSystemoneBoxRequest(t *testing.T) {
	for _, path := range decideSystemonePaths {
		for name, tc := range map[string]struct{ questions, want string }{
			"box alone":            {boxQuestion, `{"q1":{"instructions":"The logo.","type":"box"}}`},
			"instructions trimmed": {`{"name":"q1","type":"box","instructions":"  The logo. "}`, `{"q1":{"instructions":"The logo.","type":"box"}}`},
			"object instructions":  {`{"name":"q1","type":"box","instructions":{"z":"logo","a":[1.10,{"y":1,"x":2}]}}`, `{"q1":{"instructions":{"z":"logo","a":[1.10,{"y":1,"x":2}]},"type":"box"}}`},
			"array instructions":   {`{"name":"q1","type":"box","instructions":["logo",{"d":1,"c":2}]}`, `{"q1":{"instructions":["logo",{"d":1,"c":2}],"type":"box"}}`},
			"null criteria":        {`{"name":"q1","type":"box","instructions":"The logo.","criteria":null}`, `{"q1":{"instructions":"The logo.","type":"box"}}`},
			"mixed in order": {`{"name":"p","instructions":"Is it?"},{"name":"q1","type":"box","instructions":"The logo."},{"name":"c","choices":["a","b"]},{"name":"s","levels":["lo","hi"]}`,
				`{"c":{"criteria":{"a":"a","b":"b"},"type":"choice"},"p":{"instructions":"Is it?","type":"noul"},"q1":{"instructions":"The logo.","type":"box"},"s":{"criteria":["lo","hi"],"type":"score"}}`},
		} {
			t.Run(path+"/"+name, func(t *testing.T) {
				rig := newDecideSystemoneRig(t, 200, `{"answers":{}}`, func(s *sessionfs.ExtensionDecideSystemoneSettings) { s.Path = path; s.MaxInputBytes = 100000 })
				rig.call(t, `{"input":"x",`+boxAssets+`"questions":[`+tc.questions+`]}`)
				if len(rig.requests) != 1 {
					t.Fatalf("%d requests", len(rig.requests))
				}
				wantBody := `{"assets":{"img-1":{"type":"image","format":"png","data_base64":"AAAA"}},"model":"m-1","questions":` + tc.want + `,"state":"x"}`
				if string(rig.requests[0].Raw) != wantBody {
					t.Fatalf("body:\n%s\nwant:\n%s", rig.requests[0].Raw, wantBody)
				}
			})
		}
	}
}

func TestDecideSystemoneBoxRefusals(t *testing.T) {
	for name, tc := range map[string]struct{ question, want string }{
		"no instructions":      {`{"name":"a","type":"box"}`, `needs instructions that name the target`},
		"null instructions":    {`{"name":"a","type":"box","instructions":null}`, `needs instructions that name the target`},
		"blank instructions":   {`{"name":"a","type":"box","instructions":"   "}`, `schema`},
		"empty object":         {`{"name":"a","type":"box","instructions":{}}`, `needs instructions that name the target`},
		"empty array":          {`{"name":"a","type":"box","instructions":[]}`, `needs instructions that name the target`},
		"criteria object":      {`{"name":"a","type":"box","instructions":"logo","criteria":{"true":"x"}}`, `takes no criteria, choices or levels`},
		"empty criteria":       {`{"name":"a","type":"box","instructions":"logo","criteria":{}}`, `takes no criteria, choices or levels`},
		"criteria list":        {`{"name":"a","type":"box","instructions":"logo","criteria":["x"]}`, `takes no criteria, choices or levels`},
		"choices":              {`{"name":"a","type":"box","instructions":"logo","choices":["x","y"]}`, `takes no criteria, choices or levels`},
		"levels":               {`{"name":"a","type":"box","instructions":"logo","levels":["x","y"]}`, `takes no criteria, choices or levels`},
		"criteria and choices": {`{"name":"a","type":"box","instructions":"logo","criteria":{"x":"y"},"choices":["x"]}`, `has criteria and also choices or levels`},
		"unknown type":         {`{"name":"a","type":"image","instructions":"logo"}`, `schema`},
		"upper case is fixed":  {`{"name":"a","type":"BOX"}`, `needs instructions that name the target`},
	} {
		t.Run(name, func(t *testing.T) {
			keyReads := 0
			rig := newDecideSystemoneRig(t, 200, `{"answers":{}}`, func(s *sessionfs.ExtensionDecideSystemoneSettings) { s.MaxInputBytes = 100000 })
			tool, err := newDecideSystemoneTool(rig.settings, func(string) string { keyReads++; return "k" }, rig.server.Client())
			if err != nil {
				t.Fatal(err)
			}
			catalog, err := tools.NewCatalog([]tools.Tool{tool})
			if err != nil {
				t.Fatal(err)
			}
			output := executeCatalogCall(t, catalog, "decide_systemone", `{"input":"x","questions":[`+tc.question+`]}`)
			decideSystemoneRefused(t, output, len(rig.requests), tc.want)
			if keyReads != 0 {
				t.Fatalf("the key was read %d time(s) for a refused call", keyReads)
			}
		})
	}
}

// A type is never taken to be a box: without it the question is what its fields say.
func TestDecideSystemoneBoxIsNeverInferred(t *testing.T) {
	rig := newDecideSystemoneRig(t, 200, `{"answers":{"a":{"type":"noul","noul":0.5}}}`, nil)
	output := rig.call(t, `{"input":"x","questions":[{"name":"a","instructions":"Where is the logo?"}]}`)
	if got := decideSystemoneWireQuestion(t, rig, "a")["type"]; got != "noul" || rig.requests[0].Images != "" || !strings.Contains(output, `"type":"predicate"`) {
		t.Fatalf("type %v, images %q:\n%s", got, rig.requests[0].Images, output)
	}
}

func TestDecideSystemoneImagesHeaderFollowsTheQuestions(t *testing.T) {
	const plain = `{"name":"a","instructions":"?"}`
	for _, path := range decideSystemonePaths {
		for _, longContext := range []bool{false, true} {
			for name, tc := range map[string]struct {
				questions string
				images    string
			}{
				"no box":        {plain, ""},
				"box only":      {boxQuestion, "true"},
				"box and other": {plain + `,` + boxQuestion, "true"},
				"box last":      {`{"name":"c","choices":["a","b"]},` + boxQuestion, "true"},
			} {
				t.Run(fmt.Sprintf("%s/long=%t/%s", path, longContext, name), func(t *testing.T) {
					rig := newDecideSystemoneRig(t, 200, `{"answers":{}}`, func(s *sessionfs.ExtensionDecideSystemoneSettings) { s.Path = path; s.LongContext = longContext })
					// A box needs its image; the question that is not a box sends none.
					assets := ""
					if strings.Contains(tc.questions, `"box"`) {
						assets = boxAssets
					}
					rig.call(t, `{"input":"x",`+assets+`"questions":[`+tc.questions+`]}`)
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

func boxReply(box, rest string) string {
	return `{"answers":{"q1":{"type":"box"` + box + rest + `}}}`
}

func TestDecideSystemoneBoxAnswers(t *testing.T) {
	const edges = `{"top":0.1,"bottom":0.9,"left":0.2,"right":0.8}`
	const usable = `{"name":"q1","type":"box","box":{"top":0.1,"bottom":0.9,"left":0.2,"right":0.8}`
	for name, tc := range map[string]struct{ reply, want string }{
		"valid without confidence":  {boxReply(`,"box":`+edges, ""), usable + `}`},
		"null confidence omitted":   {boxReply(`,"box":`+edges, `,"confidence":null`), usable + `}`},
		"valid confidence rounded":  {boxReply(`,"box":`+edges, `,"confidence":0.123456`), usable + `,"confidence":0.1235}`},
		"confidence zero":           {boxReply(`,"box":`+edges, `,"confidence":0`), usable + `,"confidence":0}`},
		"confidence one":            {boxReply(`,"box":`+edges, `,"confidence":1`), usable + `,"confidence":1}`},
		"edges kept as received":    {boxReply(`,"box":{"right":1.0,"left":0.250,"bottom":1e0,"top":0}`, ""), `{"name":"q1","type":"box","box":{"top":0,"bottom":1e0,"left":0.250,"right":1.0}}`},
		"whole image":               {boxReply(`,"box":{"top":0,"bottom":1,"left":0,"right":1}`, ""), `{"name":"q1","type":"box","box":{"top":0,"bottom":1,"left":0,"right":1}}`},
		"extra box keys ignored":    {boxReply(`,"box":{"top":0.1,"bottom":0.9,"left":0.2,"right":0.8,"extra":5}`, ""), usable + `}`},
		"probabilities not shown":   {boxReply(`,"box":`+edges, `,"probabilities":{"a":0.5}`), usable + `}`},
		"top missing":               {boxReply(`,"box":{"bottom":0.9,"left":0.2,"right":0.8}`, ""), `{"name":"q1","type":"box","status":"unusable"}`},
		"bottom null":               {boxReply(`,"box":{"top":0.1,"bottom":null,"left":0.2,"right":0.8}`, ""), `{"name":"q1","type":"box","status":"unusable"}`},
		"left string":               {boxReply(`,"box":{"top":0.1,"bottom":0.9,"left":"0.2","right":0.8}`, ""), `{"name":"q1","type":"box","status":"unusable"}`},
		"right object":              {boxReply(`,"box":{"top":0.1,"bottom":0.9,"left":0.2,"right":{}}`, ""), `{"name":"q1","type":"box","status":"unusable"}`},
		"right bool":                {boxReply(`,"box":{"top":0.1,"bottom":0.9,"left":0.2,"right":true}`, ""), `{"name":"q1","type":"box","status":"unusable"}`},
		"top below zero":            {boxReply(`,"box":{"top":-0.1,"bottom":0.9,"left":0.2,"right":0.8}`, ""), `{"name":"q1","type":"box","status":"unusable"}`},
		"bottom above one":          {boxReply(`,"box":{"top":0.1,"bottom":1.1,"left":0.2,"right":0.8}`, ""), `{"name":"q1","type":"box","status":"unusable"}`},
		"left above one":            {boxReply(`,"box":{"top":0.1,"bottom":0.9,"left":1.5,"right":2}`, ""), `{"name":"q1","type":"box","status":"unusable"}`},
		"right overflow":            {boxReply(`,"box":{"top":0.1,"bottom":0.9,"left":0.2,"right":1e999}`, ""), `{"name":"q1","type":"box","status":"unusable"}`},
		"top equals bottom":         {boxReply(`,"box":{"top":0.5,"bottom":0.5,"left":0.2,"right":0.8}`, ""), `{"name":"q1","type":"box","status":"unusable"}`},
		"top below bottom reversed": {boxReply(`,"box":{"top":0.9,"bottom":0.1,"left":0.2,"right":0.8}`, ""), `{"name":"q1","type":"box","status":"unusable"}`},
		"left equals right":         {boxReply(`,"box":{"top":0.1,"bottom":0.9,"left":0.4,"right":0.4}`, ""), `{"name":"q1","type":"box","status":"unusable"}`},
		"left after right":          {boxReply(`,"box":{"top":0.1,"bottom":0.9,"left":0.8,"right":0.2}`, ""), `{"name":"q1","type":"box","status":"unusable"}`},
		"edge key in other case":    {boxReply(`,"box":{"Top":0.1,"bottom":0.9,"left":0.2,"right":0.8}`, ""), `{"name":"q1","type":"box","status":"unusable"}`},
		"box null":                  {boxReply(`,"box":null`, ""), `{"name":"q1","type":"box","status":"unusable"}`},
		"box missing":               {boxReply(``, ""), `{"name":"q1","type":"box","status":"unusable"}`},
		"box empty object":          {boxReply(`,"box":{}`, ""), `{"name":"q1","type":"box","status":"unusable"}`},
		"box string":                {boxReply(`,"box":"0.1,0.9"`, ""), `{"name":"q1","type":"box","status":"unusable"}`},
		"box number":                {boxReply(`,"box":5`, ""), `{"name":"q1","type":"box","status":"unusable"}`},
		"box array":                 {boxReply(`,"box":[0.1,0.9,0.2,0.8]`, ""), `{"name":"q1","type":"box","status":"unusable"}`},
		"confidence string":         {boxReply(`,"box":`+edges, `,"confidence":"high"`), `{"name":"q1","type":"box","status":"unusable"}`},
		"confidence above one":      {boxReply(`,"box":`+edges, `,"confidence":1.5`), `{"name":"q1","type":"box","status":"unusable"}`},
		"confidence below zero":     {boxReply(`,"box":`+edges, `,"confidence":-0.1`), `{"name":"q1","type":"box","status":"unusable"}`},
		"refused":                   {`{"answers":{"q1":{"type":"refusal","box":` + edges + `}}}`, `{"name":"q1","type":"box","status":"refused"}`},
		"other type":                {`{"answers":{"q1":{"type":"noul","noul":0.5,"box":` + edges + `}}}`, `{"name":"q1","type":"box","status":"unusable"}`},
		"unanswered":                {`{"answers":{}}`, `{"name":"q1","type":"box","status":"missing"}`},
	} {
		t.Run(name, func(t *testing.T) {
			for _, path := range decideSystemonePaths {
				rig := newDecideSystemoneRig(t, 200, tc.reply, func(s *sessionfs.ExtensionDecideSystemoneSettings) { s.Path = path })
				got := decideSystemoneContentText(t, rig.call(t, `{"input":"x",`+boxAssets+`"questions":[`+boxQuestion+`]}`))
				if want := `{"answers":[` + tc.want + `]}`; got != want {
					t.Fatalf("%s:\n got %s\nwant %s", path, got, want)
				}
			}
		})
	}
}

// A box answer is not read as another type's answer, and a bad box spoils only itself.
func TestDecideSystemoneBoxBesideOtherQuestions(t *testing.T) {
	const questions = `{"input":"x",` + boxAssets + `"questions":[{"name":"p","instructions":"?"},` + boxQuestion + `,{"name":"c","choices":["a","b"]},{"name":"s","levels":["lo","hi"]}]}`
	rig := newDecideSystemoneRig(t, 200, `{"answers":{
 "p":{"type":"noul","noul":0.25},
 "q1":{"type":"box","box":{"top":0.9,"bottom":0.1,"left":0.2,"right":0.8}},
 "c":{"type":"choice","choice":"b","probabilities":{"a":0.25,"b":0.75},"confidence":0.75},
 "s":{"type":"score","score":0.5,"legend":{"0":"x","1":"y"}}}}`, func(s *sessionfs.ExtensionDecideSystemoneSettings) { s.MaxInputBytes = 100000 })
	got := decideSystemoneContentText(t, rig.call(t, questions))
	want := `{"answers":[{"name":"p","type":"predicate","probability":0.25},{"name":"q1","type":"box","status":"unusable"},` +
		`{"name":"c","type":"choice","choice":"b","confidence":0.75,"probabilities":{"a":0.25,"b":0.75}},` +
		`{"name":"s","type":"score","score":0.5,"level":"hi","legend":{"0":"x","1":"y"}}]}`
	if got != want {
		t.Fatalf("got %s\nwant %s", got, want)
	}
	if rig.requests[0].Images != "true" {
		t.Fatalf("images header %q", rig.requests[0].Images)
	}
}

// A non-box question never takes a box answer.
func TestDecideSystemoneBoxAnswerToAnotherQuestionIsUnusable(t *testing.T) {
	rig := newDecideSystemoneRig(t, 200, `{"answers":{"q1":{"type":"box","box":{"top":0.1,"bottom":0.9,"left":0.2,"right":0.8}}}}`, nil)
	got := decideSystemoneContentText(t, rig.call(t, `{"input":"x","questions":[{"name":"q1","instructions":"?"}]}`))
	if want := `{"answers":[{"name":"q1","type":"predicate","status":"unusable"}]}`; got != want {
		t.Fatalf("got %s want %s", got, want)
	}
}

func TestDecideSystemoneBoxCountsInstructionsInTheInputBudget(t *testing.T) {
	long := strings.Repeat("a", 200)
	rig := newDecideSystemoneRig(t, 200, `{"answers":{}}`, func(s *sessionfs.ExtensionDecideSystemoneSettings) { s.MaxInputBytes = 150 })
	output := rig.call(t, `{"input":"x",`+boxAssets+`"questions":[{"name":"q1","type":"box","instructions":"`+long+`"}]}`)
	decideSystemoneRefused(t, output, len(rig.requests), "max_input_bytes")
}

func TestDecideSystemoneBoxEdgeHelper(t *testing.T) {
	box := decideSystemoneBoxFromJSON([]byte(`{"top":0,"bottom":1,"left":0.5,"right":0.75}`))
	if box == nil || !reflect.DeepEqual([]string{string(box.Top), string(box.Bottom), string(box.Left), string(box.Right)}, []string{"0", "1", "0.5", "0.75"}) {
		t.Fatalf("box = %+v", box)
	}
	if decideSystemoneBoxFromJSON(nil) != nil {
		t.Fatal("an absent box was accepted")
	}
}
