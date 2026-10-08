package runworker

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/xjfyrh/jobforge/internal/run"
	"github.com/xjfyrh/jobforge/internal/runinput"
)

func TestManifestRejectsMixedDeploymentAndUntrustedFields(t *testing.T) {
	valid := `{"schema_version":1,"executor_version":"` + runinput.ExecutorVersion + `","profiles":[{"profile_id":"profile-v1","profile_hash":"` + strings.Repeat("a", 64) + `","adapter_id":"registered-v1"}]}`
	manifest, err := ParseManifest([]byte(valid))
	if err != nil || len(manifest.Profiles) != 1 {
		t.Fatalf("valid manifest: %v", err)
	}
	for name, value := range map[string]string{
		"duplicate_root": strings.Replace(valid, `"schema_version":1`, `"schema_version":1,"schema_version":1`, 1),
		"extra_root":     strings.Replace(valid, `"schema_version":1`, `"schema_version":1,"module":"elsewhere"`, 1),
		"extra_profile":  strings.Replace(valid, `"adapter_id":`, `"endpoint":"http://untrusted","adapter_id":`, 1),
		"null_profile":   strings.Replace(valid, `"profile-v1"`, `null`, 1),
		"wrong_version":  strings.Replace(valid, runinput.ExecutorVersion, "old-runtime", 1),
		"bool_version":   strings.Replace(valid, `"schema_version":1`, `"schema_version":true`, 1),
		"float_version":  strings.Replace(valid, `"schema_version":1`, `"schema_version":1.0`, 1),
		"uppercase_hash": strings.Replace(valid, strings.Repeat("a", 64), strings.Repeat("A", 64), 1),
		"trailing_json":  valid + "{}",
		"oversize":       valid + strings.Repeat(" ", 16384),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseManifest([]byte(value)); !errors.Is(err, run.ErrProfileUnavailable) {
				t.Fatalf("unsafe deployment accepted: %v", err)
			}
		})
	}
	for _, count := range []int{0, 2, 33} {
		invalid := manifest
		invalid.Profiles = make([]ManifestProfile, count)
		for i := range invalid.Profiles {
			invalid.Profiles[i] = manifest.Profiles[0]
		}
		raw, marshalErr := json.Marshal(invalid)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		if _, err := ParseManifest(raw); err == nil {
			t.Fatalf("empty/duplicate/excessive profiles accepted: %d", count)
		}
	}
}

func TestManifestPromptSelectorCannotUpgradeHistoricalProfiles(t *testing.T) {
	valid := `{"schema_version":2,"executor_version":"` + run.SupportApprovalExecutorVersion + `","profiles":[{"profile_id":"candidate-v2","profile_hash":"` + strings.Repeat("a", 64) + `","adapter_id":"support-agent-v1","prompt_version":"` + run.SupportAgentPromptV2 + `"}]}`
	if _, err := ParseManifest([]byte(valid)); err != nil {
		t.Fatal(err)
	}
	v3 := strings.Replace(valid, run.SupportAgentPromptV2, run.SupportAgentPromptV3, 1)
	if _, err := ParseManifest([]byte(v3)); err != nil {
		t.Fatal("explicit v3 selector rejected", err)
	}
	if _, err := ParseManifest([]byte(strings.Replace(v3, `"schema_version":2`, `"schema_version":1`, 1))); !errors.Is(err, run.ErrProfileUnavailable) {
		t.Fatal("v3 silently upgraded historical manifest", err)
	}
	for _, invalid := range []string{
		strings.Replace(valid, `"schema_version":2`, `"schema_version":1`, 1),
		strings.Replace(valid, run.SupportAgentPromptV2, run.SupportAgentPromptVersion, 1),
		strings.Replace(valid, `,"prompt_version":"`+run.SupportAgentPromptV2+`"`, "", 1),
		strings.Replace(valid, `"prompt_version":"`+run.SupportAgentPromptV2+`"`, `"prompt_version":null`, 1),
		strings.Replace(valid, run.SupportAgentPromptV2, "unregistered-module", 1),
	} {
		if _, err := ParseManifest([]byte(invalid)); !errors.Is(err, run.ErrProfileUnavailable) {
			t.Fatalf("mixed selector accepted: %v", err)
		}
	}
}
