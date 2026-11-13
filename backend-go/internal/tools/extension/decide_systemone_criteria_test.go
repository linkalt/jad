package extension

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs"
	"jad/backend-go/internal/tools"
)

var decideSystemonePaths = []string{"/v1/systemone", "/decisions"}

// decideSystemoneRefused asserts a call failed locally: invalid input, no request, and the key unread.
func decideSystemoneRefused(t *testing.T, output string, requests int, want string) {
	t.Helper()
	text := strings.ReplaceAll(output, `\"`, `"`)
	if requests != 0 || !strings.Contains(text, `"status":"invalid_input"`) || !strings.Contains(text, want) {
		t.Fatalf("want a local invalid_input containing %q, got (%d requests):\n%s", want, requests, output)
	}
}

func decideSystemoneWireQuestion(t *testing.T, rig *decideSystemoneRig, name string) map[string]any {
	t.Helper()
	if len(rig.requests) != 1 {
		t.Fatalf("%d requests sent", len(rig.requests))
	}
	return rig.requests[0].Body["questions"].(map[string]any)[name].(map[string]any)
}

func TestDecideSystemoneInstructionsAreOptionalAndStructured(t *testing.T) {
	for _, path := range decideSystemonePaths {
		for name, tc := range map[string]struct{ question, wantRaw string }{
			"string is trimmed": {`{"name":"a","instructions":"  Is it?  "}`, `"instructions":"Is it?"`},
			"absent":            {`{"name":"a"}`, `{"a":{"type":"noul"}}`},
			"null":              {`{"name":"a","instructions":null}`, `{"a":{"type":"noul"}}`},
			"object verbatim":   {`{"name":"a","instructions":{"z": " pad ", "a": [1.10, 12345678901234567890.123456789, 1e2], "m": {"y":1,"x":2}}}`, `"instructions":{"z":" pad ","a":[1.10,12345678901234567890.123456789,1e2],"m":{"y":1,"x":2}}`},
			"array verbatim":    {`{"name":"a","instructions":["b",{"d":1,"c":2}," c "]}`, `"instructions":["b",{"d":1,"c":2}," c "]`},
		} {
			t.Run(path+"/"+name, func(t *testing.T) {
				rig := newDecideSystemoneRig(t, 200, `{"answers":{"a":{"type":"noul","noul":0.5}}}`, func(s *sessionfs.ExtensionDecideSystemoneSettings) { s.Path = path })
				output := rig.call(t, `{"input":"x","questions":[`+tc.question+`]}`)
				if len(rig.requests) != 1 || !strings.Contains(output, `"probability":0.5`) {
					t.Fatalf("not answered: %s", output)
				}
				if raw := string(rig.requests[0].Raw); !strings.Contains(raw, tc.wantRaw) {
					t.Fatalf("request lacks %s:\n%s", tc.wantRaw, raw)
				}
			})
		}
	}
}

func TestDecideSystemoneBlankInstructionsAreStillRefused(t *testing.T) {
	rig := newDecideSystemoneRig(t, 200, `{"answers":{}}`, nil)
	for _, instructions := range []string{`""`, `"   "`, `5`, `true`} {
		output := rig.call(t, `{"input":"x","questions":[{"instructions":`+instructions+`}]}`)
		if !strings.Contains(output, `"status":"invalid_input"`) || !strings.Contains(output, "schema") {
			t.Fatalf("instructions %s: want a schema error, got:\n%s", instructions, output)
		}
	}
	if len(rig.requests) != 0 {
		t.Fatalf("%d request(s) sent", len(rig.requests))
	}
}

func TestDecideSystemonePredicateCriteria(t *testing.T) {
	for _, path := range decideSystemonePaths {
		for name, tc := range map[string]struct{ criteria, want string }{
			"true only":  {`{"true":"spam"}`, `"criteria":{"true":"spam"}`},
			"false only": {`{"false":["a",{"b":1}]}`, `"criteria":{"false":["a",{"b":1}]}`},
			"both":       {`{"true":"yes","false":{"k":"no"}}`, `"criteria":{"true":"yes","false":{"k":"no"}}`},
			"null value": {`{"true":null,"false":"no"}`, `"criteria":{"true":null,"false":"no"}`},
			"empty":      {`{}`, `"criteria":{}`},
		} {
			t.Run(path+"/"+name, func(t *testing.T) {
				rig := newDecideSystemoneRig(t, 200, `{"answers":{"a":{"type":"noul","noul":0.25}}}`, func(s *sessionfs.ExtensionDecideSystemoneSettings) { s.Path = path })
				output := rig.call(t, `{"input":"x","questions":[{"name":"a","instructions":"?","criteria":`+tc.criteria+`}]}`)
				if len(rig.requests) != 1 || !strings.Contains(output, `"probability":0.25`) || !strings.Contains(string(rig.requests[0].Raw), tc.want) {
					t.Fatalf("bad call: %s\n%s", output, rig.requests)
				}
			})
		}
	}
	for _, criteria := range []string{`null`} {
		rig := newDecideSystemoneRig(t, 200, `{"answers":{"a":{"type":"noul","noul":0.25}}}`, nil)
		rig.call(t, `{"input":"x","questions":[{"name":"a","instructions":"?","criteria":`+criteria+`}]}`)
		if len(rig.requests) != 1 || strings.Contains(string(rig.requests[0].Raw), "criteria") {
			t.Fatalf("null criteria must be left out: %s", rig.requests[0].Raw)
		}
	}
}

func TestDecideSystemoneCriteriaRefusals(t *testing.T) {
	for name, tc := range map[string]struct{ question, want string }{
		"predicate extra key":      {`{"name":"a","instructions":"?","criteria":{"true":"x","maybe":"y"}}`, `its criteria take only true and false, not "maybe"`},
		"labels without type":      {`{"name":"a","instructions":"?","criteria":{"billing":"x","other":"y"}}`, `for labelled criteria set type choice`},
		"list without type":        {`{"name":"a","instructions":"?","criteria":["x","y"]}`, `for a list set type score`},
		"predicate number value":   {`{"name":"a","instructions":"?","criteria":{"true":1}}`, `the criteria for true must be text, an object, an array or null`},
		"choice with choices":      {`{"name":"a","type":"choice","instructions":"?","criteria":{"x":"y"},"choices":["x"]}`, `has criteria and also choices or levels`},
		"choice with levels":       {`{"name":"a","type":"choice","instructions":"?","criteria":{"x":"y"},"levels":["x"]}`, `has criteria and also choices or levels`},
		"score with levels":        {`{"name":"a","type":"score","instructions":"?","criteria":["x"],"levels":["x"]}`, `has criteria and also choices or levels`},
		"inferred with choices":    {`{"name":"a","instructions":"?","criteria":{"x":"y"},"choices":["x"]}`, `has criteria and also choices or levels`},
		"choice with a list":       {`{"name":"a","type":"choice","instructions":"?","criteria":["x"]}`, `its criteria must be an object of label to meaning`},
		"score with an object":     {`{"name":"a","type":"score","instructions":"?","criteria":{"x":"y"}}`, `its criteria must be a list, lowest first`},
		"choice empty":             {`{"name":"a","type":"choice","instructions":"?","criteria":{}}`, `its criteria are empty`},
		"choice blank key":         {`{"name":"a","type":"choice","instructions":"?","criteria":{"":"y"}}`, `option without a value`},
		"choice number value":      {`{"name":"a","type":"choice","instructions":"?","criteria":{"x":2}}`, `the criteria for "x" must be text`},
		"choice duplicate key":     {`{"name":"a","type":"choice","instructions":"?","criteria":{"x":"1","x":"2"}}`, `lists "x" twice`},
		"score empty":              {`{"name":"a","type":"score","instructions":"?","criteria":[]}`, `schema`},
		"score null entry":         {`{"name":"a","type":"score","instructions":"?","criteria":["x",null]}`, `criterion 2 must be text, an object or an array`},
		"score number entry":       {`{"name":"a","type":"score","instructions":"?","criteria":[1]}`, `criterion 1 must be text, an object or an array`},
		"predicate with levels":    {`{"name":"a","type":"predicate","instructions":"?","criteria":{"true":"x"},"levels":["x"]}`, `has criteria and also choices or levels`},
		"criteria is a string":     {`{"name":"a","instructions":"?","criteria":"x"}`, `schema`},
		"native choice over limit": {`{"name":"a","type":"choice","instructions":"?","criteria":` + criteriaObject(256) + `}`, `has 256 choices; the maximum is 255`},
		"native score over limit":  {`{"name":"a","type":"score","instructions":"?","criteria":` + criteriaList(11) + `}`, `has 11 levels; the maximum is 10`},
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

func criteriaObject(count int) string {
	parts := make([]string, count)
	for i := range parts {
		parts[i] = fmt.Sprintf("%q:%q", "k"+strconv.Itoa(i), "m")
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func criteriaList(count int) string {
	parts := make([]string, count)
	for i := range parts {
		parts[i] = fmt.Sprintf("%q", "c"+strconv.Itoa(i))
	}
	return "[" + strings.Join(parts, ",") + "]"
}

func TestDecideSystemoneNativeChoiceCriteria(t *testing.T) {
	for _, path := range decideSystemonePaths {
		t.Run(path+"/order values and answer", func(t *testing.T) {
			reply := `{"answers":{"team":{"type":"choice","choice":"zeta","probabilities":{"alpha":0.1,"zeta":0.8,"mid":0.1},"confidence":0.8}}}`
			rig := newDecideSystemoneRig(t, 200, reply, func(s *sessionfs.ExtensionDecideSystemoneSettings) { s.Path = path })
			output := rig.call(t, `{"input":"x","questions":[{"name":"team","type":"CHOICE","instructions":"?","criteria":{"zeta":"last letter","alpha":{"when":[1.50,2]},"mid":null,"other":["a"]}}]}`)
			want := `"criteria":{"zeta":"last letter","alpha":{"when":[1.50,2]},"mid":null,"other":["a"]},"instructions":"?","type":"choice"`
			if len(rig.requests) != 1 || !strings.Contains(string(rig.requests[0].Raw), want) {
				t.Fatalf("request lacks declared order %s:\n%s", want, rig.requests[0].Raw)
			}
			if !strings.Contains(output, `"choice":"zeta","confidence":0.8,"probabilities":{"zeta":0.8,"alpha":0.1,"mid":0.1}`) {
				t.Fatalf("answer: %s", output)
			}
		})
		t.Run(path+"/answer outside the criteria is unusable", func(t *testing.T) {
			rig := newDecideSystemoneRig(t, 200, `{"answers":{"a":{"type":"choice","choice":"nope"}}}`, func(s *sessionfs.ExtensionDecideSystemoneSettings) { s.Path = path })
			output := rig.call(t, `{"input":"x","questions":[{"name":"a","type":"choice","instructions":"?","criteria":{"x":"1"}}]}`)
			if !strings.Contains(output, `"status":"unusable"`) {
				t.Fatalf("want unusable: %s", output)
			}
		})
	}
	for _, count := range []int{1, 255} {
		t.Run(strconv.Itoa(count)+" keys", func(t *testing.T) {
			rig := newDecideSystemoneRig(t, 200, `{"answers":{"a":{"type":"choice","choice":"k0"}}}`, func(s *sessionfs.ExtensionDecideSystemoneSettings) { s.MaxInputBytes = 100000 })
			output := rig.call(t, `{"input":"x","questions":[{"name":"a","type":"choice","instructions":"?","criteria":`+criteriaObject(count)+`}]}`)
			criteria := decideSystemoneWireQuestion(t, rig, "a")["criteria"].(map[string]any)
			if len(criteria) != count || !strings.Contains(output, `"choice":"k0"`) {
				t.Fatalf("%d keys sent; %s", len(criteria), output)
			}
		})
	}
}

func TestDecideSystemoneNativeScoreCriteria(t *testing.T) {
	for _, path := range decideSystemonePaths {
		t.Run(path, func(t *testing.T) {
			reply := `{"answers":{"s":{"type":"score","score":1.7,"legend":{"0":"low","1":{"k":1},"2":["hi"]},"probabilities":{"0":0.1,"1":0.2,"2":0.7},"confidence":0.7}}}`
			rig := newDecideSystemoneRig(t, 200, reply, func(s *sessionfs.ExtensionDecideSystemoneSettings) { s.Path = path })
			output := rig.call(t, `{"input":"x","questions":[{"name":"s","type":"score","instructions":"?","criteria":["low",{"b":1.0,"a":2},["x"]]}]}`)
			if len(rig.requests) != 1 || !strings.Contains(string(rig.requests[0].Raw), `"criteria":["low",{"b":1.0,"a":2},["x"]]`) {
				t.Fatalf("request: %s", rig.requests[0].Raw)
			}
			if !strings.Contains(output, `"score":1.7,"confidence":0.7,"probabilities":{"0":0.1,"1":0.2,"2":0.7},"legend":{"0":"low","1":{"k":1},"2":["hi"]}`) || strings.Contains(output, `"level"`) {
				t.Fatalf("answer must carry no invented level label: %s", output)
			}
		})
	}
	for _, count := range []int{1, 10} {
		t.Run(strconv.Itoa(count)+" entries", func(t *testing.T) {
			rig := newDecideSystemoneRig(t, 200, `{"answers":{"s":{"type":"score","score":0}}}`, nil)
			output := rig.call(t, `{"input":"x","questions":[{"name":"s","type":"score","instructions":"?","criteria":`+criteriaList(count)+`}]}`)
			if criteria := decideSystemoneWireQuestion(t, rig, "s")["criteria"].([]any); len(criteria) != count || !strings.Contains(output, `"score":0`) {
				t.Fatalf("%d sent; %s", len(criteria), output)
			}
		})
	}
}

func TestDecideSystemoneSingleOptionQuestions(t *testing.T) {
	for _, path := range decideSystemonePaths {
		t.Run(path, func(t *testing.T) {
			reply := `{"answers":{"c":{"type":"choice","choice":"only","probabilities":{"only":1}},"s":{"type":"score","score":0,"probabilities":{"0":1}}}}`
			rig := newDecideSystemoneRig(t, 200, reply, func(s *sessionfs.ExtensionDecideSystemoneSettings) { s.Path = path })
			output := rig.call(t, `{"input":"x","questions":[{"name":"c","instructions":"?","choices":["only"]},{"name":"s","instructions":"?","levels":["Single"]}]}`)
			questions := rig.requests[0].Body["questions"].(map[string]any)
			if fmt.Sprint(questions["c"].(map[string]any)["criteria"]) != "map[only:only]" || fmt.Sprint(questions["s"].(map[string]any)["criteria"]) != "[Single]" {
				t.Fatalf("request: %s", rig.requests[0].Raw)
			}
			if !strings.Contains(output, `"choice":"only","probabilities":{"only":1}`) || !strings.Contains(output, `"score":0,"level":"Single","probabilities":{"Single":1}`) {
				t.Fatalf("answer: %s", output)
			}
		})
	}
}

// Without the raw call text (a nested call), structured values are encoded from the decoded
// arguments: still valid, with sorted keys.
func TestDecideSystemoneStructuredValuesWithoutTheRawArguments(t *testing.T) {
	rig := newDecideSystemoneRig(t, 200, `{"answers":{"a":{"type":"choice","choice":"b"}}}`, nil)
	built, err := newDecideSystemoneTool(rig.settings, func(string) string { return "k" }, rig.server.Client())
	if err != nil {
		t.Fatal(err)
	}
	var arguments map[string]any
	decoder := json.NewDecoder(strings.NewReader(`{"input":"x","questions":[{"name":"a","type":"choice","instructions":{"z":1.50,"a":2},"criteria":{"b":"x","a":{"n":12345678901234567890}}}]}`))
	decoder.UseNumber()
	if err := decoder.Decode(&arguments); err != nil {
		t.Fatal(err)
	}
	output, err := built.Execute(t.Context(), arguments, tools.ExecutionContext{CallID: "c"})
	if err != nil || !strings.Contains(output, `"choice":"b"`) {
		t.Fatalf("%v: %s", err, output)
	}
	if raw := string(rig.requests[0].Raw); !strings.Contains(raw, `"criteria":{"a":{"n":12345678901234567890},"b":"x"},"instructions":{"a":2,"z":1.50}`) {
		t.Fatalf("request: %s", raw)
	}
}

// Raw arguments that no longer describe the decoded value (a hook rewrite) are not trusted.
func TestDecideSystemoneIgnoresRawArgumentsThatDisagreeWithTheCall(t *testing.T) {
	rig := newDecideSystemoneRig(t, 200, `{"answers":{"a":{"type":"choice","choice":"b"}}}`, nil)
	built, err := newDecideSystemoneTool(rig.settings, func(string) string { return "k" }, rig.server.Client())
	if err != nil {
		t.Fatal(err)
	}
	var arguments map[string]any
	decoder := json.NewDecoder(strings.NewReader(`{"input":"x","questions":[{"name":"a","type":"choice","instructions":"?","criteria":{"b":"new"}}]}`))
	decoder.UseNumber()
	if err := decoder.Decode(&arguments); err != nil {
		t.Fatal(err)
	}
	stale := json.RawMessage(`{"input":"x","questions":[{"name":"a","type":"choice","instructions":"?","criteria":{"z":"old","b":"new"}}]}`)
	if _, err := built.Execute(t.Context(), arguments, tools.ExecutionContext{CallID: "c", RawArguments: stale}); err != nil {
		t.Fatal(err)
	}
	if raw := string(rig.requests[0].Raw); !strings.Contains(raw, `"criteria":{"b":"new"}`) || strings.Contains(raw, "old") {
		t.Fatalf("request: %s", raw)
	}
}
