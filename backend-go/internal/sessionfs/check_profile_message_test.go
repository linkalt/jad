package sessionfs

import (
	"strings"
	"testing"

	"jad/backend-go/internal/sessionfs/layouttest"
)

func TestProfileMessageTargetsExcludeTheCallerAndDeniedProfiles(t *testing.T) {
	got := ProfileMessageTargets([]string{"main", "reviewer", "team/coder", "team/writer"}, "main", []string{"team/coder", "absent"})
	if strings.Join(got, ",") != "reviewer,team/writer" {
		t.Fatalf("targets = %v", got)
	}
}

func checkProfileMessageSession(t *testing.T, profiles []string, deny []any) CheckReport {
	t.Helper()
	files := map[string]map[string]any{}
	for _, id := range profiles {
		files[id] = map[string]any{"models": []string{"openai/default"}, "model_default": "openai/default", "toolset": "coding/default"}
	}
	fixture := layouttest.Write(t, layouttest.Spec{
		Profiles: files,
		Native:   map[string]any{"profile_message": map[string]any{"deny_profiles": deny}},
	})
	config, err := Load(LoadOptions{WorkingDir: fixture.Root, RootSettingPath: fixture.RootSetting, Environment: map[string]string{}})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	report, err := CheckLayout(config.Roots, nil)
	if err != nil {
		t.Fatalf("CheckLayout: %v", err)
	}
	return report
}

func TestCheckReportsAProfileMessageWithoutTargets(t *testing.T) {
	cases := map[string]struct {
		profiles []string
		deny     []any
		want     []string
	}{
		"one profile":       {[]string{"dev/a"}, nil, []string{"dev/a"}},
		"everything denied": {[]string{"dev/a", "dev/b"}, []any{"dev/a", "dev/b"}, []string{"dev/a", "dev/b"}},
		"a target remains":  {[]string{"dev/a", "dev/b"}, nil, nil},
		"one side denied":   {[]string{"dev/a", "dev/b"}, []any{"dev/b"}, []string{"dev/a"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			report := checkProfileMessageSession(t, tc.profiles, tc.deny)
			if len(report.Problems) != len(tc.want) {
				t.Fatalf("problems = %+v, want one per profile %v", report.Problems, tc.want)
			}
			for index, profile := range tc.want {
				message := report.Problems[index].Message
				if !strings.Contains(message, ErrProfileMessageNoTargets.Error()) || !strings.Contains(message, "(profile "+profile+")") || !strings.Contains(message, "session-1.json") {
					t.Errorf("message = %q, want the no-target error naming the setting path and profile %s", message, profile)
				}
			}
		})
	}
}
