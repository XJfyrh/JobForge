// Package diagnosis runs only the frozen, read-only S5 decision experiment.
package diagnosis

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/xjfyrh/jobforge/internal/jsonstrict"
	"github.com/xjfyrh/jobforge/internal/run"
)

// ErrInvalid deliberately contains no credentials or model content.
var ErrInvalid = errors.New("invalid diagnosis input")

const (
	maxRequestBytes        = 128 * 1024
	maxResponseBytes       = 1024 * 1024
	maxOutputTokens  int64 = 16384
	maxInputTokens   int64 = 1048576
)

// Trial identifies one immutable, non-retryable experimental observation.
type Trial struct {
	ID            string `json:"trial_id"`
	CaseID        string `json:"case_id"`
	Group         string `json:"group"`
	Repetition    int    `json:"repetition"`
	RequestFile   string `json:"request_file"`
	RequestSHA256 string `json:"request_sha256"`
}

// Parent binds the one approved classification amendment to its completed prefix.
type Parent struct {
	ExperimentID    string `json:"experiment_id"`
	ManifestSHA256  string `json:"manifest_sha256"`
	CompletedTrials int    `json:"completed_trials"`
}

// Manifest contains no gold or execution-selected URL, model, or policy.
type Manifest struct {
	SchemaVersion   int               `json:"schema_version"`
	ExperimentID    string            `json:"experiment_id"`
	DatabaseName    string            `json:"database_name"`
	DatabaseID      string            `json:"database_id"`
	CreatedAt       string            `json:"created_at"`
	RunnerSHA256    string            `json:"runner_sha256"`
	Sources         map[string]string `json:"sources"`
	MaxInputTokens  int64             `json:"max_input_tokens"`
	MaxOutputTokens int64             `json:"max_output_tokens"`
	PriorCost       int64             `json:"prior_cost_microyuan"`
	CostCap         int64             `json:"cost_cap_microyuan"`
	BatchCap        int64             `json:"batch_cap_microyuan"`
	Pricing         run.Pricing       `json:"pricing"`
	Trials          []Trial           `json:"trials"`
	Parent          *Parent           `json:"parent,omitempty"`
}

// Digest preserves the exact request, response and manifest bytes.
func Digest(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// LoadManifest verifies all requests before registration or any paid send.
func LoadManifest(path string) (Manifest, []byte, error) {
	var m Manifest
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) > 256*1024 || jsonstrict.Decode(raw, &m) != nil {
		return m, nil, ErrInvalid
	}
	costCap, trials := int64(20000000), 240
	if m.SchemaVersion == 3 {
		costCap, trials = 28000000, 20
	}
	if (m.SchemaVersion != 1 && m.SchemaVersion != 2 && m.SchemaVersion != 3) || !run.ValidIdentifier(m.ExperimentID) ||
		!strings.HasPrefix(m.DatabaseName, "jobforge_s5_diagnosis_") || uuid.Validate(m.DatabaseID) != nil ||
		m.MaxInputTokens != maxInputTokens || m.MaxOutputTokens != maxOutputTokens ||
		m.PriorCost < 0 || m.PriorCost >= m.CostCap || m.CostCap != costCap || m.BatchCap != 4000000 ||
		m.Pricing.Denominator != 1000000 || m.Pricing.InputMissMicroyuan != 2000000 ||
		m.Pricing.InputHitMicroyuan != 40000 || m.Pricing.OutputMicroyuan != 8000000 ||
		!run.ValidHash(m.Pricing.Hash) || !run.ValidHash(m.RunnerSHA256) || len(m.Trials) != trials {
		return m, nil, ErrInvalid
	}
	if m.SchemaVersion != 2 && m.Parent != nil || m.SchemaVersion == 2 && (m.Parent == nil ||
		m.Parent.CompletedTrials != 15 || !run.ValidIdentifier(m.Parent.ExperimentID) ||
		m.Parent.ExperimentID == m.ExperimentID || !run.ValidHash(m.Parent.ManifestSHA256)) {
		return m, nil, ErrInvalid
	}
	for _, key := range []string{"original_evidence", "original_registration", "prior_ledger", "preflight", "preparation_source"} {
		if !run.ValidHash(m.Sources[key]) {
			return m, nil, ErrInvalid
		}
	}
	if m.SchemaVersion == 3 {
		for _, key := range []string{"candidate_sources", "input_verification", "budget_authorisation", "historical_package", "historical_freeze"} {
			if !run.ValidHash(m.Sources[key]) {
				return m, nil, ErrInvalid
			}
		}
	}
	for index, t := range m.Trials {
		repetition, caseIndex := index/80+1, index%80/4
		group := string("ABCD"[(caseIndex+repetition-1+index%4)%4])
		caseID := fmt.Sprintf("S5V3-%03d", caseIndex+1)
		if m.SchemaVersion == 3 {
			repetition, group, caseID = 1, "V5", fmt.Sprintf("S5V3-%03d", index+1)
		}
		if t.CaseID != caseID || t.Group != group || t.Repetition != repetition ||
			t.ID != fmt.Sprintf("%s-%s-%d", caseID, group, repetition) {
			return m, nil, ErrInvalid
		}
		if _, err := requestBytes(filepath.Dir(path), t); err != nil {
			return m, nil, err
		}
	}
	return m, raw, nil
}

type chatRequest struct {
	Model    string `json:"model"`
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
	Thinking struct {
		Type string `json:"type"`
	} `json:"thinking"`
	Stream         *bool `json:"stream"`
	MaxTokens      int64 `json:"max_tokens"`
	ResponseFormat struct {
		Type string `json:"type"`
	} `json:"response_format"`
	Temperature     *int   `json:"temperature"`
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
}

func requestBytes(root string, t Trial) ([]byte, error) {
	if t.RequestFile != "requests/"+t.CaseID+"-"+t.Group+".json" {
		return nil, ErrInvalid
	}
	raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(t.RequestFile)))
	var p chatRequest
	if err != nil || len(raw) > maxRequestBytes || Digest(raw) != t.RequestSHA256 || jsonstrict.Decode(raw, &p) != nil {
		return nil, ErrInvalid
	}
	mode, effort := "disabled", ""
	if t.Group == "B" || t.Group == "V5" {
		mode, effort = "enabled", "high"
	}
	if p.Model != "deepseek-flash" || p.Thinking.Type != mode || p.ReasoningEffort != effort ||
		p.Stream == nil || *p.Stream || p.MaxTokens != maxOutputTokens || p.ResponseFormat.Type != "json_object" ||
		len(p.Messages) != 2 || p.Messages[0].Role != "system" || p.Messages[1].Role != "user" ||
		len(p.Messages[0].Content)+len(p.Messages[1].Content) > 65536 {
		return nil, ErrInvalid
	}
	if t.Group == "V5" {
		var facts map[string]json.RawMessage
		var fields map[string]json.RawMessage
		if jsonstrict.Decode(raw, &fields) != nil || fields["temperature"] != nil ||
			!strings.Contains(p.Messages[0].Content, "OUTPUT ENVELOPE FOR THIS STEP:") ||
			jsonstrict.Decode([]byte(p.Messages[1].Content), &facts) != nil || string(facts["final_stage"]) != "true" {
			return nil, ErrInvalid
		}
		for _, key := range []string{"previous_tools", "remaining_tools", "completed_requests", "policy_navigation", "policy_retrieval", "draft"} {
			if _, exists := facts[key]; exists {
				return nil, ErrInvalid
			}
		}
	} else if p.Temperature == nil || *p.Temperature != 0 || !strings.Contains(p.Messages[0].Content, "DIAGNOSTIC FINAL-ONLY PROTOCOL:") {
		return nil, ErrInvalid
	}
	return raw, nil
}

// VerifyBinary prevents a frozen experiment from silently selecting another runner.
func VerifyBinary(expected string) error {
	path, err := os.Executable()
	if err != nil {
		return ErrInvalid
	}
	raw, err := os.ReadFile(path)
	if err != nil || Digest(raw) != expected {
		return ErrInvalid
	}
	return nil
}

func writeNew(path string, value any) error {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return ErrInvalid
	}
	return writeRawNew(path, append(raw, '\n'))
}

func writeRawNew(path string, raw []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return errors.New("evidence file creation failed")
	}
	_, writeErr := f.Write(raw)
	syncErr := f.Sync()
	closeErr := f.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil {
		return errors.New("evidence file persistence failed")
	}
	return nil
}
