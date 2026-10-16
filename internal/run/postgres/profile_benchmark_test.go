package postgres

import (
	"encoding/json"
	"os"
	"testing"

	agentrun "github.com/xjfyrh/jobforge/internal/run"
)

func BenchmarkBatchAuditProfileValidation(b *testing.B) {
	raw, err := os.ReadFile("../../../tests/testdata/support-profile-schema-vectors.json")
	if err != nil {
		b.Fatal(err)
	}
	var definitions map[string]struct {
		Definition agentrun.SupportDefinition `json:"definition"`
	}
	if err := json.Unmarshal(raw, &definitions); err != nil {
		b.Fatal(err)
	}
	profile, err := agentrun.BuildSupportProfile("benchmark-profile", definitions["s5-agent-v9"].Definition)
	if err != nil {
		b.Fatal(err)
	}
	raw, err = json.Marshal(profile)
	if err != nil {
		b.Fatal(err)
	}
	for _, cached := range []bool{false, true} {
		name := "per_chat"
		if cached {
			name = "per_profile"
		}
		b.Run(name, func(b *testing.B) {
			for b.Loop() {
				profiles := make(map[string]agentrun.Profile)
				for range 40 {
					p, exists := profiles[profile.Hash]
					if !cached || !exists {
						if json.Unmarshal(raw, &p) != nil || p.ValidateAuditPolicy() != nil {
							b.Fatal("invalid profile")
						}
						profiles[p.Hash] = p
					}
					if p.ID != profile.ID {
						b.Fatal("profile binding changed")
					}
				}
			}
		})
	}
}
