package httpapi

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
)

func equivalenceBodies() []string {
	png := base64.StdEncoding.EncodeToString(append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 64)...))
	pdf := base64.StdEncoding.EncodeToString(append([]byte("%PDF-1.4\n"), make([]byte, 64)...))
	img := `{"type":"image","data":"` + png + `"}`
	file := `{"type":"file","name":"a.pdf","data":"` + pdf + `"}`
	return []string{
		``, `   `, `not json`, `{`, `{"input":`, `{"input":"a"}{"input":"b"}`, `{"input":"a"} x`,
		`5`, `[]`, `null`, `"x"`, `true`, `{}`, `{"model":"m"}`, `{"input":null}`, `{"input":5}`,
		`{"input":""}`, `{"input":"  "}`, `{"input":"hi"}`, `{"input":"h\u00e9\n\"q\""}`, `{"input":"hi","input":"second"}`,
		` {"input" : "hi" , "model" : " m " , "supports_interaction" : true } `,
		`{"input":"hi","model":""}`, `{"input":"hi","model":5}`, `{"input":"hi","model":null}`,
		`{"input":"hi","supports_interaction":"yes"}`, `{"input":"hi","supports_interaction":false}`,
		`{"in\u0070ut":"escaped key"}`, `{"Input":"wrong case"}`, `{"input":"hi","extra":{"a":[1,2,{"b":"}\\"]}}}`,
		`{"input":[]}`, `{"input":[null]}`, `{"input":[5]}`, `{"input":["x"]}`, `{"input":[[]]}`, `{"input":[{}]}`,
		`{"input":[{"type":"text","text":"hi"}]}`,
		`{"input":[{"type":"text","text":"  "}]}`,
		`{"input":[{"type":"text"}]}`, `{"input":[{"type":"text","text":null}]}`, `{"input":[{"type":"text","text":5}]}`,
		`{"input":[{"type":5,"text":"x"}]}`, `{"input":[{"type":null,"text":"x"}]}`,
		`{"input":[{"TYPE":"text","text":"case"}]}`, `{"input":[{"type":"text","Text":"case"}]}`, `{"input":[{"type":"text","text":"a","extra":1}]}`,
		`{"input":[{"type":"text","text":"a","extra":[1,{"x":"}"}]}]}`,
		`{"input":[{"type":"text","text":"a","TEXT":"b"}]}`,
		`{"input":[{"type":"text","text":"a","text":null}]}`,
		`{"input":[{"type":"video","text":"a"}]}`,
		`{"input":[{"type":"text","text":"\u003cb\u003e & \ud83d\ude00 \ud800 bad"}]}`,
		"{\"input\":[{\"type\":\"text\",\"text\":\"raw \u00e9 \xff utf8\"}]}",
		`{"input":[` + img + `]}`, `{"input":[` + img + `,` + img + `]}`, `{"input":[` + img + `,` + img + `,` + img + `]}`,
		`{"input":[{"type":"image"}]}`, `{"input":[{"type":"image","data":"zzz"}]}`, `{"input":[{"type":"image","data":"` + pdf + `"}]}`,
		`{"input":[{"type":"image","data":"data:image/png;base64,` + png + `"}]}`,
		`{"input":[{"type":"image","data":"` + png + `","name":"x"}]}`,
		`{"input":[` + file + `]}`, `{"input":[` + file + `,` + file + `]}`, `{"input":[` + file + `,` + file + `,` + file + `]}`,
		`{"input":[{"type":"file","data":"` + pdf + `"}]}`, `{"input":[{"type":"file","name":"a.pdf"}]}`,
		`{"input":[{"type":"file","name":"..","data":"` + pdf + `"}]}`, `{"input":[{"type":"file","name":"d\\x.pdf","data":"` + pdf + `"}]}`,
		`{"input":[{"type":"file","name":"a.pdf","data":"` + png + `"}]}`,
		`{"input":[{"type":"text","text":"look"},` + img + `,` + file + `],"model":"m"}`,
		`{"input":[{"type":"text","text":"look"},{"type":"bogus"}]}`,
		`{"input":[{"type":"text","text":"ok"},5]}`,
		`{"input":[{"type":"text","text":"ok"},{"type":"image","data":5}]}`,
		"{\"input\":\"hi\"}\n", "\t{\"input\":[ {\"type\" : \"text\" , \"text\":\"sp\"} ]}",
	}
}

func TestReadRunBodyMatchesTheTwoPassParser(t *testing.T) {
	for _, limits := range []RunInputLimits{
		{MaxImages: 2, MaxImageBytes: 4096, MaxFiles: 2, MaxFileBytes: 4096},
		{MaxImages: 1, MaxImageBytes: 16, MaxFiles: 1, MaxFileBytes: 16},
		{},
	} {
		for index, body := range equivalenceBodies() {
			for _, base := range []int64{1 << 20, 100} {
				name := fmt.Sprintf("%d/%d/%d", limits.MaxImages, index, base)
				want, wantErr := oldReadRunBodyFromRaw([]byte(body), base, limits)
				request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
				got, gotErr := readRunBody(httptest.NewRecorder(), request, 1<<30, base, limits)
				if (wantErr == nil) != (gotErr == nil) {
					t.Fatalf("%s %.80q: err = %v, want %v", name, body, gotErr, wantErr)
				}
				if wantErr != nil {
					var a, b *StatusError
					if !errors.As(wantErr, &a) || !errors.As(gotErr, &b) || a.Code != b.Code || a.Message != b.Message {
						t.Fatalf("%s %.80q: err = %v, want %v", name, body, gotErr, wantErr)
					}
					continue
				}
				if !bytes.Equal(got.Input, want.Input) || got.Model != want.Model || got.SupportsInteraction != want.SupportsInteraction {
					t.Fatalf("%s %.80q: got %+v, want %+v", name, body, got, want)
				}
			}
		}
	}
}

// The two-pass parser allocated about 7.75 times the body for a maximum request. The one-pass
// reader allocates about 3.75 times (the body, its joined copy, the output message and the decoded
// attachments); 4.5 leaves room for the runtime and fails if a copy of the body comes back.
func TestReadRunBodyAllocationStaysNearFourTimesTheBody(t *testing.T) {
	limits := RunInputLimits{MaxImages: 8, MaxImageBytes: 5 << 20, MaxFiles: 4, MaxFileBytes: 10 << 20}
	payload := maxRunBody(t, limits)
	h := &Handler{maxBodyBytes: 1 << 20, runInputLimits: limits}
	request := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(payload))
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	if _, err := readRunBody(httptest.NewRecorder(), request, h.runBodyLimit(), h.maxBodyBytes, limits); err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)
	allocated := after.TotalAlloc - before.TotalAlloc
	if limit := uint64(len(payload)) * 9 / 2; allocated > limit {
		t.Fatalf("reading a %d byte body allocated %d bytes, limit %d", len(payload), allocated, limit)
	}
}
