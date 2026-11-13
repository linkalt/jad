package sessionfs

import (
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs/layouttest"
)

func TestLoadSessionConfigPersistenceMeta(t *testing.T) {
	cases := []struct {
		setting string
		want    PersistenceMeta
		wantErr string
	}{
		{setting: `{}`, want: PersistenceMeta{UserTimestamp: true, MaxBytes: 4096}},
		{setting: `{"persistence":{"meta":null}}`, want: PersistenceMeta{UserTimestamp: true, MaxBytes: 4096}},
		{setting: `{"persistence":{"meta":{}}}`, want: PersistenceMeta{UserTimestamp: true, MaxBytes: 4096}},
		{setting: `{"persistence":{"meta":{"user_timestamp":null,"max_bytes":null}}}`, want: PersistenceMeta{UserTimestamp: true, MaxBytes: 4096}},
		{setting: `{"persistence":{"meta":{"user_timestamp":false}}}`, want: PersistenceMeta{UserTimestamp: false, MaxBytes: 4096}},
		{setting: `{"persistence":{"meta":{"max_bytes":64}}}`, want: PersistenceMeta{UserTimestamp: true, MaxBytes: 64}},
		{setting: `{"persistence":{"meta":{"max_bytes":65536}}}`, want: PersistenceMeta{UserTimestamp: true, MaxBytes: 65536}},
		{setting: `{"persistence":{"meta":{"max_bytes":63}}}`, wantErr: "persistence.meta.max_bytes must be a safe integer between 64 and 65536"},
		{setting: `{"persistence":{"meta":{"max_bytes":65537}}}`, wantErr: "persistence.meta.max_bytes must be a safe integer between 64 and 65536"},
		{setting: `{"persistence":{"meta":{"max_bytes":100.5}}}`, wantErr: "persistence.meta.max_bytes must be a safe integer"},
		{setting: `{"persistence":{"meta":{"max_bytes":"4096"}}}`, wantErr: "persistence.meta.max_bytes must be a safe integer"},
		{setting: `{"persistence":{"meta":{"user_timestamp":"true"}}}`, wantErr: "persistence.meta.user_timestamp must be a boolean"},
		{setting: `{"persistence":{"meta":{"user_timestamp":1}}}`, wantErr: "persistence.meta.user_timestamp must be a boolean"},
		{setting: `{"persistence":{"meta":{"user_timestamps":false}}}`, wantErr: "persistence.meta.user_timestamps is not supported"},
		{setting: `{"persistence":{"meta":true}}`, wantErr: "persistence.meta must be an object"},
		{setting: `{"persistence":{"meta":[]}}`, wantErr: "persistence.meta must be an object"},
	}
	for _, test := range cases {
		t.Run(test.setting, func(t *testing.T) {
			var settings map[string]json.RawMessage
			if err := json.Unmarshal([]byte(test.setting), &settings); err != nil {
				t.Fatal(err)
			}
			location, _ := layoutLocation(t, layouttest.Spec{RawSettings: settings})
			config, err := LoadSessionConfig(location, "", "")
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("err = %v, want %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadSessionConfig: %v", err)
			}
			if config.Persistence.Meta != test.want {
				t.Fatalf("meta = %+v, want %+v", config.Persistence.Meta, test.want)
			}
		})
	}
}

func TestPersistenceMetaSchemaMatchesTheLoader(t *testing.T) {
	data, err := os.ReadFile("../../../sample/schemes/setting.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties struct {
			Persistence struct {
				Properties struct {
					Meta struct {
						Type                 string `json:"type"`
						AdditionalProperties bool   `json:"additionalProperties"`
						Properties           map[string]struct {
							Type    string          `json:"type"`
							Default json.RawMessage `json:"default"`
							Minimum *int64          `json:"minimum"`
							Maximum *int64          `json:"maximum"`
						} `json:"properties"`
					} `json:"meta"`
				} `json:"properties"`
			} `json:"persistence"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	meta := schema.Properties.Persistence.Properties.Meta
	var keys []string
	for key := range meta.Properties {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	want := append([]string(nil), persistenceMetaKeys...)
	sort.Strings(want)
	if meta.Type != "object" || meta.AdditionalProperties || !reflect.DeepEqual(keys, want) {
		t.Fatalf("schema meta %s keys %v (additionalProperties %v), loader accepts %v", meta.Type, keys, meta.AdditionalProperties, want)
	}
	if timestamp := meta.Properties["user_timestamp"]; timestamp.Type != "boolean" || string(timestamp.Default) != "true" || !defaultMetaUserTimestamp {
		t.Errorf("schema user_timestamp = %+v", timestamp)
	}
	maxBytes := meta.Properties["max_bytes"]
	var def int64
	if err := json.Unmarshal(maxBytes.Default, &def); err != nil || maxBytes.Type != "integer" || def != defaultMetaMaxBytes || maxBytes.Minimum == nil || *maxBytes.Minimum != minMetaMaxBytes || maxBytes.Maximum == nil || *maxBytes.Maximum != maxMetaMaxBytes {
		t.Errorf("schema max_bytes = %+v (default %s)", maxBytes, maxBytes.Default)
	}
}
