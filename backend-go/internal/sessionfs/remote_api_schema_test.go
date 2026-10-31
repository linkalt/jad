package sessionfs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The schema documents listen_unix_mode as the range 384-438 and says that a group or
// other write bit is refused. Each mode in that range must get the answer the description
// states: refused exactly when mode & 0o022 is set.
func TestRemoteAPIListenUnixModeSchemaMatchesTheLoader(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "sample", "schemes", "root-setting.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties map[string]struct {
			Properties map[string]struct {
				Properties map[string]struct {
					Minimum     int    `json:"minimum"`
					Maximum     int    `json:"maximum"`
					Description string `json:"description"`
				} `json:"properties"`
			} `json:"properties"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	mode := schema.Properties["server"].Properties["remote_api"].Properties["listen_unix_mode"]
	if mode.Minimum != 384 || mode.Maximum != 438 {
		t.Fatalf("listen_unix_mode range = %d-%d, want 384-438", mode.Minimum, mode.Maximum)
	}
	if !strings.Contains(mode.Description, "group or other write") {
		t.Fatalf("listen_unix_mode description does not say group or other write is refused: %q", mode.Description)
	}
	for value := mode.Minimum; value <= mode.Maximum; value++ {
		_, err := remoteAPI(t, `{"enabled":true,"tokens_file":"t.json","listen_unix":"/run/jad.sock","listen_unix_mode":`+strconv.Itoa(value)+`}`)
		refused := value&0o022 != 0
		if refused != (err != nil) {
			t.Fatalf("listen_unix_mode %d (%#o): refused = %v, want %v (err %v)", value, value, err != nil, refused, err)
		}
	}
}
