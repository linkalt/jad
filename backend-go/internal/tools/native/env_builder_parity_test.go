package native

import (
	"reflect"
	"testing"

	"jad/backend-go/internal/sessionfs"
)

// bash and process build the child environment separately; for the same inputs they must
// agree, in particular on env_deny removing a name that env or env_file set.
func TestBashAndProcessEnvironmentBuildersAgree(t *testing.T) {
	base := []string{
		"PATH=/usr/bin:/bin", "HOME=/tmp", "LANG=C", "FIXTURE_SECRET=ambient",
		"JAD_API_TOKEN=server", "UNLISTED=x", "DENIED_AMBIENT=a", "TZ=UTC",
	}
	env := map[string]string{
		"NO_COLOR": "1", "FROM_FILE": "v", "DENIED_CONFIGURED": "c", "FIXTURE_SECRET": "configured",
		"JAD_API_TOKEN": "configured", "PATH": "/custom/bin",
	}
	deny := []string{"DENIED_AMBIENT", "DENIED_CONFIGURED"}
	fromBash := buildNativeBashEnvironment(base, sessionfs.NativeBashSettings{Env: env, EnvDeny: deny}, "FIXTURE_SECRET")
	fromProcess := buildNativeProcessEnvironment(base, env, "FIXTURE_SECRET", deny)
	if !reflect.DeepEqual(fromBash, fromProcess) {
		t.Fatalf("bash environment %v\nprocess environment %v", fromBash, fromProcess)
	}
	for _, item := range fromBash {
		for _, forbidden := range []string{"DENIED_AMBIENT=", "DENIED_CONFIGURED=", "FIXTURE_SECRET=", "JAD_API_TOKEN=", "UNLISTED="} {
			if len(item) >= len(forbidden) && item[:len(forbidden)] == forbidden {
				t.Fatalf("environment keeps %s", item)
			}
		}
	}
}
