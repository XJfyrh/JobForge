package diagnosis

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func probeManifestFixture(t *testing.T, schema int) (string, Manifest) {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "requests"), 0700); err != nil {
		t.Fatal(err)
	}
	m := testManifest()
	m.SchemaVersion, m.CostCap, m.Trials = schema, 28000000, nil
	m.DatabaseName, m.DatabaseID = "jobforge_s5_diagnosis_contract", uuid.NewString()
	m.RunnerSHA256 = strings.Repeat("a", 64)
	for _, key := range []string{"original_evidence", "original_registration", "prior_ledger", "preflight", "preparation_source", "candidate_sources", "input_verification"} {
		m.Sources[key] = m.RunnerSHA256
	}
	cases, groups, repeats := []string{"DEV-003", "DEV-004", "DEV-016", "DEV-017", "DEV-026", "DEV-027"}, []string{"V8", "V8C"}, 1
	if schema == 5 {
		cases, groups, repeats = []string{"DEV-016", "DEV-017", "DEV-020"}, []string{"C1", "C2"}, 3
		m.Probe = &ProbePlan{CaseIDs: cases, Repetitions: repeats}
		m.Sources["probe_plan"] = m.RunnerSHA256
	}
	for repetition := 1; repetition <= repeats; repetition++ {
		for pair, cid := range cases {
			for position := range 2 {
				group := groups[(pair+position+repetition-1)%2]
				raw := encoded(t, map[string]any{"model": "deepseek-flash", "stream": false,
					"max_tokens": 16384, "thinking": map[string]any{"type": "enabled"}, "reasoning_effort": "high",
					"response_format": map[string]any{"type": "json_object"},
					"messages": []any{map[string]any{"role": "system", "content": "This is the final adjudication, independent of any investigation draft."},
						map[string]any{"role": "user", "content": `{"final_stage":true}`}}})
				trial := Trial{ID: fmt.Sprintf("%s-%s-%d", cid, group, repetition), CaseID: cid, Group: group, Repetition: repetition,
					RequestFile: "requests/" + cid + "-" + group + ".json", RequestSHA256: Digest(raw)}
				if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(trial.RequestFile)), raw, 0600); err != nil {
					t.Fatal(err)
				}
				m.Trials = append(m.Trials, trial)
			}
		}
	}
	return root, m
}

func TestContractProbeIsExactlyTwelveClosedFinalRequests(t *testing.T) {
	root, m := probeManifestFixture(t, 4)
	path := filepath.Join(root, "manifest.json")
	check := func(ok bool) {
		t.Helper()
		if err := os.WriteFile(path, encoded(t, m), 0600); err != nil {
			t.Fatal(err)
		}
		_, _, err := LoadManifest(path)
		if (err == nil) != ok {
			t.Fatalf("accepted=%v expected=%v: %v", err == nil, ok, err)
		}
	}
	check(true)
	for _, mutation := range []string{"cohort", "order", "group", "repeat", "source", "parent", "cap", "stage"} {
		original := encoded(t, m)
		switch mutation {
		case "cohort":
			m.Trials = m.Trials[:10]
		case "order":
			m.Trials[0], m.Trials[1] = m.Trials[1], m.Trials[0]
		case "group":
			m.Trials[0].Group = "B"
		case "repeat":
			m.Trials[0].Repetition = 2
		case "source":
			delete(m.Sources, "input_verification")
		case "parent":
			m.Parent = &Parent{}
		case "cap":
			m.BatchCap++
		case "stage":
			m.CostCap++
		}
		check(false)
		m = Manifest{}
		if err := json.Unmarshal(original, &m); err != nil {
			t.Fatal(err)
		}
	}
	trial := m.Trials[0]
	requestPath := filepath.Join(root, filepath.FromSlash(trial.RequestFile))
	original, err := os.ReadFile(requestPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []string{"temperature", "capacity", "thinking", "effort", "stream", "draft", "remaining_tools", "final_stage", "prompt"} {
		var body map[string]any
		if err := json.Unmarshal(original, &body); err != nil {
			t.Fatal(err)
		}
		switch mutation {
		case "temperature":
			body["temperature"] = nil
		case "capacity":
			body["max_tokens"] = 1024
		case "thinking":
			body["thinking"] = map[string]any{"type": "disabled"}
		case "effort":
			delete(body, "reasoning_effort")
		case "stream":
			body["stream"] = true
		case "prompt":
			body["messages"].([]any)[0].(map[string]any)["content"] = "arbitrary protocol"
		default:
			facts := map[string]any{"final_stage": true, mutation: false}
			body["messages"].([]any)[1].(map[string]any)["content"] = string(encoded(t, facts))
		}
		raw := encoded(t, body)
		m.Trials[0].RequestSHA256 = Digest(raw)
		if err := os.WriteFile(requestPath, raw, 0600); err != nil {
			t.Fatal(err)
		}
		check(false)
	}
}

func TestContractProbeProtocolHealth(t *testing.T) {
	for _, tc := range []struct {
		outcomes []string
		stopAt   int
	}{
		{[]string{"invalid_final", "final", "final", "final"}, 0},
		{[]string{"invalid_final", "final", "incomplete_output"}, 3},
		{[]string{"final", "invalid_final", "incomplete_output"}, 3},
		{[]string{"final", "final", "final", "invalid_final", "final", "invalid_final"}, 0},
		{[]string{"final", "final", "final", "invalid_final", "incomplete_output"}, 5},
	} {
		var h protocolHealth
		stopped := 0
		for i, outcome := range tc.outcomes {
			if h.stop(i+1, outcome) {
				stopped = i + 1
				break
			}
		}
		if stopped != tc.stopAt {
			t.Fatalf("%v stopped at %d; want %d", tc.outcomes, stopped, tc.stopAt)
		}
	}
}

func TestConfiguredAblationManifestBindsCohortAndRepetitions(t *testing.T) {
	root, m := probeManifestFixture(t, 5)
	path := filepath.Join(root, "manifest.json")
	check := func(ok bool) {
		t.Helper()
		if err := os.WriteFile(path, encoded(t, m), 0600); err != nil {
			t.Fatal(err)
		}
		_, _, err := LoadManifest(path)
		if (err == nil) != ok {
			t.Fatalf("accepted=%v expected=%v: %v", err == nil, ok, err)
		}
	}
	check(true)
	for _, mutation := range []string{"missing_plan", "duplicate_case", "unknown_case", "missing_target", "repetitions", "order", "source", "old_schema", "group"} {
		original := encoded(t, m)
		switch mutation {
		case "missing_plan":
			m.Probe = nil
		case "duplicate_case":
			m.Probe.CaseIDs[1] = m.Probe.CaseIDs[0]
		case "unknown_case":
			m.Probe.CaseIDs[1] = "DEV-041"
		case "missing_target":
			m.Probe.CaseIDs[0] = "DEV-015"
		case "repetitions":
			m.Probe.Repetitions = 2
		case "order":
			m.Trials[6], m.Trials[7] = m.Trials[7], m.Trials[6]
		case "source":
			delete(m.Sources, "probe_plan")
		case "old_schema":
			m.SchemaVersion = 4
		case "group":
			m.Trials[0].Group = "V8"
		}
		check(false)
		m = Manifest{}
		if err := json.Unmarshal(original, &m); err != nil {
			t.Fatal(err)
		}
	}
}

func TestConfiguredProbeRequiresLiveAuthorizationDeadline(t *testing.T) {
	root, m := probeManifestFixture(t, 5)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	binary, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	m.RunnerSHA256 = Digest(binary)
	path, credentials := filepath.Join(root, "manifest.json"), filepath.Join(root, "credentials.json")
	if err := os.WriteFile(path, encoded(t, m), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(credentials, []byte(`{"tenants":{"tenant-north":{"deepseek_api_key":"synthetic-test-key"},"tenant-south":{"deepseek_api_key":"synthetic-test-key"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	for _, expired := range []bool{false, true} {
		ctx := context.Background()
		if expired {
			var cancel context.CancelFunc
			ctx, cancel = context.WithDeadline(ctx, time.Now().Add(-time.Second))
			defer cancel()
		}
		// A nil pool makes any accidental database/dispatch access fail this test.
		if err := Execute(ctx, nil, path, credentials, io.Discard); !errors.Is(err, ErrInvalid) {
			t.Fatalf("inactive authorization reached execution: %v", err)
		}
	}
}
