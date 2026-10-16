package diagnosis

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestRequestIdentityAndConfigurationClosed(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "requests"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, group := range []string{"A", "B", "C", "D"} {
		body := map[string]any{"model": "deepseek-flash", "stream": false, "max_tokens": 16384,
			"temperature": 0, "response_format": map[string]any{"type": "json_object"},
			"thinking": map[string]any{"type": "disabled"},
			"messages": []any{map[string]any{"role": "system", "content": "DIAGNOSTIC FINAL-ONLY PROTOCOL:"}, map[string]any{"role": "user", "content": "facts"}}}
		if group == "B" {
			body["thinking"] = map[string]any{"type": "enabled"}
			body["reasoning_effort"] = "high"
		}
		trial := Trial{ID: "case-" + group + "-1", CaseID: "case", Group: group, Repetition: 1, RequestFile: "requests/case-" + group + ".json"}
		path := filepath.Join(root, filepath.FromSlash(trial.RequestFile))
		raw := encoded(t, body)
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		trial.RequestSHA256 = Digest(raw)
		if _, err := requestBytes(root, trial, false); err != nil {
			t.Fatal(group, err)
		}
		trial.RequestSHA256 = strings.Repeat("0", 64)
		if _, err := requestBytes(root, trial, false); err == nil {
			t.Fatal("changed bytes accepted")
		}
		for _, key := range []string{"stream", "temperature"} {
			previous := body[key]
			delete(body, key)
			raw = encoded(t, body)
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			trial.RequestSHA256 = Digest(raw)
			if _, err := requestBytes(root, trial, false); err == nil {
				t.Fatal("missing parameter accepted", key)
			}
			body[key] = previous
		}
		body["max_tokens"] = 1024
		raw, _ = json.Marshal(body)
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		trial.RequestSHA256 = Digest(raw)
		if _, err := requestBytes(root, trial, false); err == nil {
			t.Fatal("unequal capacity accepted")
		}
		trial.RequestFile = "../secret.json"
		if _, err := requestBytes(root, trial, false); err == nil {
			t.Fatal("path escaped request package")
		}
	}
}

func TestManifestExactCohortAndRotatingSchedule(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "requests"), 0700); err != nil {
		t.Fatal(err)
	}
	m := testManifest()
	m.SchemaVersion, m.DatabaseName, m.DatabaseID = 1, "jobforge_s5_diagnosis_fixture", uuid.NewString()
	m.RunnerSHA256 = strings.Repeat("a", 64)
	m.Sources = map[string]string{}
	for _, key := range []string{"original_evidence", "original_registration", "prior_ledger", "preflight", "preparation_source"} {
		m.Sources[key] = m.RunnerSHA256
	}
	for index := range 240 {
		repetition, caseIndex := index/80+1, index%80/4
		group := string("ABCD"[(caseIndex+repetition-1+index%4)%4])
		caseID := fmt.Sprintf("S5V3-%03d", caseIndex+1)
		body := map[string]any{"model": "deepseek-flash", "stream": false, "temperature": 0, "max_tokens": 16384,
			"thinking": map[string]any{"type": "disabled"}, "response_format": map[string]any{"type": "json_object"},
			"messages": []any{map[string]any{"role": "system", "content": "DIAGNOSTIC FINAL-ONLY PROTOCOL:"}, map[string]any{"role": "user", "content": "facts"}}}
		if group == "B" {
			body["thinking"] = map[string]any{"type": "enabled"}
			body["reasoning_effort"] = "high"
		}
		raw := encoded(t, body)
		trial := Trial{ID: fmt.Sprintf("%s-%s-%d", caseID, group, repetition), CaseID: caseID, Group: group, Repetition: repetition,
			RequestFile: "requests/" + caseID + "-" + group + ".json", RequestSHA256: Digest(raw)}
		m.Trials[index] = trial
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(trial.RequestFile)), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(root, "manifest.json")
	check := func(wantOK bool) {
		t.Helper()
		if err := os.WriteFile(path, encoded(t, m), 0600); err != nil {
			t.Fatal(err)
		}
		_, _, err := LoadManifest(path)
		if (err == nil) != wantOK {
			t.Fatalf("manifest accepted=%v expected=%v", err == nil, wantOK)
		}
	}
	check(true)
	m.Trials[0], m.Trials[1] = m.Trials[1], m.Trials[0]
	check(false)
	m.Trials[0], m.Trials[1] = m.Trials[1], m.Trials[0]
	m.Trials[0].CaseID = "S5V3-021"
	check(false)
}

func TestV5ReplayManifestAndRequestAreClosed(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "requests"), 0700); err != nil {
		t.Fatal(err)
	}
	m := testManifest()
	m.SchemaVersion, m.CostCap, m.Trials = 3, 28000000, nil
	m.DatabaseName, m.DatabaseID = "jobforge_s5_diagnosis_replay", uuid.NewString()
	m.RunnerSHA256 = strings.Repeat("a", 64)
	for _, key := range []string{"original_evidence", "original_registration", "prior_ledger", "preflight", "preparation_source", "candidate_sources", "input_verification", "budget_authorisation", "historical_package", "historical_freeze"} {
		m.Sources[key] = m.RunnerSHA256
	}
	body := map[string]any{"model": "deepseek-flash", "stream": false, "max_tokens": 16384,
		"thinking": map[string]any{"type": "enabled"}, "reasoning_effort": "high",
		"response_format": map[string]any{"type": "json_object"},
		"messages": []any{map[string]any{"role": "system", "content": "OUTPUT ENVELOPE FOR THIS STEP:"},
			map[string]any{"role": "user", "content": `{"final_stage":true}`}}}
	for index := range 20 {
		cid := fmt.Sprintf("S5V3-%03d", index+1)
		raw := encoded(t, body)
		trial := Trial{ID: cid + "-V5-1", CaseID: cid, Group: "V5", Repetition: 1,
			RequestFile: "requests/" + cid + "-V5.json", RequestSHA256: Digest(raw)}
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(trial.RequestFile)), raw, 0600); err != nil {
			t.Fatal(err)
		}
		m.Trials = append(m.Trials, trial)
	}
	path := filepath.Join(root, "manifest.json")
	check := func(wantOK bool) {
		t.Helper()
		if err := os.WriteFile(path, encoded(t, m), 0600); err != nil {
			t.Fatal(err)
		}
		_, _, err := LoadManifest(path)
		if (err == nil) != wantOK {
			t.Fatalf("manifest accepted=%v expected=%v", err == nil, wantOK)
		}
	}
	check(true)
	for _, mutation := range []string{"cap", "batch", "source", "parent", "repetition", "group", "cohort", "order"} {
		original := encoded(t, m)
		switch mutation {
		case "cap":
			m.CostCap = 20000000
		case "batch":
			m.BatchCap++
		case "source":
			delete(m.Sources, "input_verification")
		case "parent":
			m.Parent = &Parent{}
		case "repetition":
			m.Trials[0].Repetition = 2
		case "group":
			m.Trials[0].Group = "B"
		case "cohort":
			m.Trials = m.Trials[:19]
		case "order":
			m.Trials[0], m.Trials[1] = m.Trials[1], m.Trials[0]
		}
		check(false)
		m = Manifest{}
		if err := json.Unmarshal(original, &m); err != nil {
			t.Fatal(err)
		}
	}
	trial := &m.Trials[0]
	requestPath := filepath.Join(root, filepath.FromSlash(trial.RequestFile))
	for _, mutation := range []string{"temperature", "null_temperature", "capacity", "thinking", "effort", "stream", "draft", "remaining_tools", "final_stage"} {
		var changed map[string]any
		if err := json.Unmarshal(encoded(t, body), &changed); err != nil {
			t.Fatal(err)
		}
		switch mutation {
		case "temperature":
			changed["temperature"] = 0
		case "null_temperature":
			changed["temperature"] = nil
		case "capacity":
			changed["max_tokens"] = 1024
		case "thinking":
			changed["thinking"] = map[string]any{"type": "disabled"}
		case "effort":
			delete(changed, "reasoning_effort")
		case "stream":
			changed["stream"] = true
		default:
			facts := map[string]any{"final_stage": true, mutation: false}
			changed["messages"].([]any)[1].(map[string]any)["content"] = string(encoded(t, facts))
		}
		raw := encoded(t, changed)
		if err := os.WriteFile(requestPath, raw, 0600); err != nil {
			t.Fatal(err)
		}
		trial.RequestSHA256 = Digest(raw)
		check(false)
	}
}
