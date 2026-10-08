// Package runworker coordinates the fixed executor under control-plane leases.
// It never schedules steps or authorizes business effects independently.
package runworker

import (
	"bytes"
	"encoding/json"
	"io"
	"os"

	"github.com/xjfyrh/jobforge/internal/run"
	"github.com/xjfyrh/jobforge/internal/runinput"
)

// ManifestPath is part of the installed image contract, never task input.
const ManifestPath = "/etc/jobforge/executor.json"

// Manifest binds immutable profiles to adapters installed in the same image.
type Manifest struct {
	SchemaVersion   int               `json:"schema_version"`
	ExecutorVersion string            `json:"executor_version"`
	Profiles        []ManifestProfile `json:"profiles"`
}

// ManifestProfile contains no endpoint, credential, or dynamic module name.
type ManifestProfile struct {
	ProfileID     string `json:"profile_id"`
	ProfileHash   string `json:"profile_hash"`
	AdapterID     string `json:"adapter_id"`
	PromptVersion string `json:"prompt_version,omitempty"`
}

// LoadManifest reads only the fixed, bounded, deployment-owned file.
func LoadManifest() (Manifest, error) {
	f, err := os.Open(ManifestPath)
	if err != nil {
		return Manifest{}, run.ErrProfileUnavailable
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, 16385))
	if err != nil {
		return Manifest{}, run.ErrProfileUnavailable
	}
	return ParseManifest(data)
}

// ParseManifest is shared by deployment checks and deterministic tests.
func ParseManifest(data []byte) (Manifest, error) {
	var manifest Manifest
	if run.ValidateStepJSON(data, 16384) != nil {
		return manifest, run.ErrProfileUnavailable
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&manifest) != nil || manifest.Validate() != nil {
		return Manifest{}, run.ErrProfileUnavailable
	}
	var shape struct {
		Profiles []map[string]json.RawMessage `json:"profiles"`
	}
	_ = json.Unmarshal(data, &shape)
	for _, fields := range shape.Profiles {
		expected := 3
		if manifest.SchemaVersion == 2 {
			expected = 4
		}
		if len(fields) != expected {
			return Manifest{}, run.ErrProfileUnavailable
		}
	}
	return manifest, nil
}

// Validate rejects mixed versions, missing identities, and duplicate profiles.
func (m Manifest) Validate() error {
	if (m.SchemaVersion != 1 && m.SchemaVersion != 2) || (m.ExecutorVersion != runinput.ExecutorVersion && m.ExecutorVersion != run.SupportFixedComparisonExecutorVersion && m.ExecutorVersion != run.SupportAgentExecutorVersion && m.ExecutorVersion != run.SupportRecoveryExecutorVersion && m.ExecutorVersion != run.SupportApprovalExecutorVersion) || len(m.Profiles) < 1 || len(m.Profiles) > 32 {
		return run.ErrProfileUnavailable
	}
	seen := make(map[string]bool, len(m.Profiles))
	for _, p := range m.Profiles {
		validPrompt := (p.AdapterID == "support-agent-v1" && (p.PromptVersion == run.SupportAgentPromptV2 || p.PromptVersion == run.SupportAgentPromptV3) && m.ExecutorVersion == run.SupportApprovalExecutorVersion) || (p.AdapterID == "support-fixed-v1" && p.PromptVersion == run.SupportPromptVersion && m.ExecutorVersion == run.SupportFixedComparisonExecutorVersion)
		if m.SchemaVersion == 1 && p.PromptVersion != "" || m.SchemaVersion == 2 && !validPrompt {
			return run.ErrProfileUnavailable
		}
		if !runinput.ExecutorMatchesAdapter(m.ExecutorVersion, p.AdapterID) || !run.ValidIdentifier(p.ProfileID) || !run.ValidHash(p.ProfileHash) || !run.ValidIdentifier(p.AdapterID) || seen[p.ProfileID] {
			return run.ErrProfileUnavailable
		}
		seen[p.ProfileID] = true
	}
	return nil
}

func (m Manifest) profile(id, hash string) (ManifestProfile, error) {
	for _, p := range m.Profiles {
		if p.ProfileID == id && p.ProfileHash == hash {
			return p, nil
		}
	}
	return ManifestProfile{}, run.ErrProfileUnavailable
}
