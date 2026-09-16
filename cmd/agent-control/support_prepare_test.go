package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/xjfyrh/jobforge/internal/run"
	"github.com/xjfyrh/jobforge/internal/runworker"
)

func supportPrepareFixture(t *testing.T) (supportPrepareOptions, supportSource) {
	t.Helper()
	repo, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(repo, "api", "support", "profile-v1", "fixtures.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Profile run.Profile `json:"profile"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	d, err := run.DecodeSupportDefinition(fixture.Profile.Definition)
	if err != nil {
		t.Fatal(err)
	}
	for path, target := range map[string]*string{
		"api/support/v1/schema.json":                           &d.Program.ProposalSchemaSHA256,
		"python/jobforge_agent/support_adapter.py":             &d.Program.PromptSHA256,
		"examples/support-agent/runtime/dataset-manifest.json": &d.Resources.RuntimeManifestSHA256,
		"examples/support-agent/runtime/seed.json":             &d.Resources.SeedSHA256,
	} {
		data, err := os.ReadFile(filepath.Join(repo, filepath.FromSlash(path)))
		if err != nil {
			t.Fatal(err)
		}
		*target = supportSHA256(data)
	}
	d.Program.AdapterSourceSHA256, err = supportAdapterSHA256(repo)
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	receipt := []byte("synthetic unit-test receipt; not a production review\n")
	if err := os.WriteFile(filepath.Join(tmp, "receipt.txt"), receipt, 0600); err != nil {
		t.Fatal(err)
	}
	d.Price.SourceSHA256 = supportSHA256(receipt)
	r := supportReceipt{Path: "receipt.txt", SHA256: supportSHA256(receipt)}
	source := supportSource{SchemaVersion: 1, Definition: d, BuildReceipt: r, DataReview: r, ScoringReview: r, PriceSnapshot: r}
	o := supportPrepareOptions{Repo: repo, Source: filepath.Join(tmp, "source.json"), ProfileID: "support-prepare-test-v1", WorkerID: "support-prepare-test-worker",
		BatchID: "00000000-0000-4000-8000-000000000001", BatchKey: "support-prepare-test-batch", NorthID: "00000000-0000-4000-8000-000000000002",
		SouthID: "00000000-0000-4000-8000-000000000003", ValidFrom: "2026-09-16T12:00:00Z", Out: filepath.Join(tmp, "prepared"), BatchCostMicroyuan: 5000000}
	if err := os.WriteFile(o.Source, supportJSON(source), 0600); err != nil {
		t.Fatal(err)
	}
	return o, source
}

func TestPrepareSupportFixedArtifactsAndDisabledRegistration(t *testing.T) {
	o, _ := supportPrepareFixture(t)
	files, err := prepareSupportFiles(o)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 6 {
		t.Fatalf("files=%d", len(files))
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(filepath.Dir(o.Out), name), content, 0600); err != nil {
			t.Fatal(err)
		}
	}
	disabled, err := readDeployment(filepath.Join(filepath.Dir(o.Out), "control.disabled.json"))
	if err != nil {
		t.Fatal(err)
	}
	enabled, err := readDeployment(filepath.Join(filepath.Dir(o.Out), "control.enabled.json"))
	if err != nil {
		t.Fatal(err)
	}
	if disabled.Profiles[0].Executable || !enabled.Profiles[0].Executable {
		t.Fatal("enablement selection lost")
	}
	enabled.EnabledProfiles = []string{}
	enabled.Profiles[0].Executable = false
	if !reflect.DeepEqual(disabled, enabled) {
		t.Fatal("enabled file changed immutable configuration")
	}
	if disabled.Workers[0].Capacity != 1 || disabled.ProfileCapacity != 1 || disabled.TenantCapacity != 1 {
		t.Fatal("capacity not fixed at one")
	}
	for i, budget := range disabled.Budgets {
		wantFamilies := int64(20)
		if i == 0 {
			wantFamilies = 40
		}
		if budget.Limits != supportBudget(wantFamilies) || budget.ValidUntil.Sub(budget.ValidFrom) != 6*time.Hour {
			t.Fatal("batch limits or validity drifted")
		}
	}
	manifest, err := runworker.ParseManifest(files["executor.json"])
	if err != nil || manifest.Profiles[0].ProfileHash != disabled.Profiles[0].Hash {
		t.Fatal("executor manifest mismatch", err)
	}
	var launch supportLaunch
	if err := json.Unmarshal(files["launch.json"], &launch); err != nil {
		t.Fatal(err)
	}
	if len(launch.Cases) != 40 || len(launch.ConfigSHA256) != 4 || launch.Cases[0].CaseID != "DEV-001" || launch.Cases[39].CaseID != "DEV-040" {
		t.Fatal("case registration changed")
	}
	for name, digest := range launch.ConfigSHA256 {
		if supportSHA256(files[name]) != digest {
			t.Fatal("configuration digest mismatch")
		}
	}
	lines := bytes.Split(bytes.TrimSpace(files["rows.jsonl"]), []byte{'\n'})
	if len(lines) != 40 {
		t.Fatal("row count")
	}
	for i, line := range lines {
		var row struct {
			supportCase
			Status string `json:"status"`
		}
		if json.Unmarshal(line, &row) != nil || row.Status != "unattempted" || row.supportCase != launch.Cases[i] {
			t.Fatal("initial row identity mismatch")
		}
	}
	again, err := prepareSupportFiles(o)
	if err != nil || !reflect.DeepEqual(files, again) {
		t.Fatal("prepare is not deterministic", err)
	}
}

func TestPrepareSupportAgentVersionAndOperatorBudget(t *testing.T) {
	o, source := supportPrepareFixture(t)
	raw, err := os.ReadFile(filepath.Join(o.Repo, "deploy", "support-agent.source.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	var example supportSource
	if json.Unmarshal(raw, &example) != nil {
		t.Fatal("invalid agent source example")
	}
	priceReceipt := source.Definition.Price.SourceSHA256
	source.Definition = example.Definition
	source.Definition.Price.SourceSHA256 = priceReceipt
	o.ValidFrom, o.BatchCostMicroyuan = "2026-09-17T12:00:00Z", 20000000
	if err := os.WriteFile(o.Source, supportJSON(source), 0600); err != nil {
		t.Fatal(err)
	}
	files, err := prepareSupportFiles(o)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(filepath.Dir(o.Out), "agent-enabled.json")
	if err := os.WriteFile(path, files["control.enabled.json"], 0600); err != nil {
		t.Fatal(err)
	}
	config, err := readDeployment(path)
	if err != nil {
		t.Fatal(err)
	}
	if config.Profiles[0].Strategy != run.SupportAgentStrategy || config.Profiles[0].ExecutorVersion != run.SupportAgentExecutorVersion {
		t.Fatal("S2 lost its registered version")
	}
	for _, budget := range config.Budgets {
		if budget.Limits.CostMicroyuan != 20000000 {
			t.Fatal("operator cap was not retained")
		}
	}
	manifest, err := runworker.ParseManifest(files["executor.json"])
	if err != nil || manifest.ExecutorVersion != run.SupportAgentExecutorVersion || manifest.Profiles[0].AdapterID != "support-agent-v1" {
		t.Fatalf("S2 manifest: %v", err)
	}
}

func TestPrepareSupportOfflineAndNeverOverwritesRows(t *testing.T) {
	o, _ := supportPrepareFixture(t)
	t.Setenv("JOBFORGE_AGENT_DSN", "invalid-secret-dsn")
	t.Setenv("JOBFORGE_AGENT_CONFIG", "nonexistent-secret-config")
	t.Setenv("JOBFORGE_AGENT_BUSINESS_KEYS", "invalid-secret")
	args := []string{"prepare-support", "--source", o.Source, "--repo", o.Repo, "--profile-id", o.ProfileID, "--worker-id", o.WorkerID,
		"--batch-id", o.BatchID, "--batch-key", o.BatchKey, "--north-account-id", o.NorthID, "--south-account-id", o.SouthID,
		"--valid-from", o.ValidFrom, "--out", o.Out}
	if err := command(t.Context(), args); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(o.Out, "rows.jsonl")
	prior := []byte("existing attempted rows must survive")
	if err := os.WriteFile(path, prior, 0600); err != nil {
		t.Fatal(err)
	}
	if err := command(t.Context(), args); err == nil {
		t.Fatal("existing output replaced")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(prior, after) {
		t.Fatal("existing rows changed")
	}
}

func TestPrepareSupportRejectsUnreviewedSourcesAndInvalidBatch(t *testing.T) {
	for _, name := range []string{"receipt-digest", "source-digest", "missing-identity", "non-utc", "inside-repo", "duplicate-account"} {
		t.Run(name, func(t *testing.T) {
			o, source := supportPrepareFixture(t)
			switch name {
			case "receipt-digest":
				source.BuildReceipt.SHA256 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			case "source-digest":
				source.Definition.Program.PromptSHA256 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			case "missing-identity":
				o.BatchID = ""
			case "non-utc":
				o.ValidFrom = "2026-09-16T20:00:00+08:00"
			case "inside-repo":
				o.Out = filepath.Join(o.Repo, "prepared")
			case "duplicate-account":
				o.NorthID = o.BatchID
			}
			if err := os.WriteFile(o.Source, supportJSON(source), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := prepareSupportFiles(o); err == nil {
				t.Fatal("invalid preparation accepted")
			}
		})
	}
}

func TestPrepareSupportReducedBatchCapPreservesOtherLimits(t *testing.T) {
	o, _ := supportPrepareFixture(t)
	o.BatchCostMicroyuan = 2846003
	files, err := prepareSupportFiles(o)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"control.disabled.json", "control.enabled.json"} {
		config, err := readDeployment(deploymentFile(t, string(files[name])))
		if err != nil {
			t.Fatal(err)
		}
		want := supportBudget(40)
		want.CostMicroyuan = o.BatchCostMicroyuan
		if config.Budgets[0].Limits != want || config.Budgets[1].Limits != supportBudget(20) ||
			config.Budgets[2].Limits != supportBudget(20) || config.Profiles[0].FamilyCostMicroyuan != 5000000 {
			t.Fatal("reduced batch cap changed another budget boundary")
		}
		var launch supportLaunch
		if json.Unmarshal(files["launch.json"], &launch) != nil || launch.ConfigSHA256[name] != supportSHA256(files[name]) {
			t.Fatal("reduced cap is not bound by the launch configuration digest")
		}
	}
}

func TestPrepareSupportRejectsInvalidCostCap(t *testing.T) {
	for _, cap := range []int64{-1, 0, 5000001} {
		o, _ := supportPrepareFixture(t)
		o.BatchCostMicroyuan = cap
		if _, err := prepareSupportFiles(o); err == nil {
			t.Fatalf("invalid cap accepted: %d", cap)
		}
		o.BatchCostMicroyuan = 5000000
		files, err := prepareSupportFiles(o)
		if err != nil {
			t.Fatal(err)
		}
		var config deployment
		if err := json.Unmarshal(files["control.disabled.json"], &config); err != nil {
			t.Fatal(err)
		}
		config.Budgets[0].Limits.CostMicroyuan = cap
		if _, err := readDeployment(deploymentFile(t, string(supportJSON(config)))); err == nil {
			t.Fatalf("invalid deployment cap accepted: %d", cap)
		}
	}
	for _, value := range []string{"bad", "1.5", "9223372036854775808"} {
		if err := prepareSupport([]string{"--batch-cost-microyuan", value}); err == nil {
			t.Fatal("invalid integer cap accepted")
		}
	}
}

func TestDeploymentRequiresAuditedSupportAndRecomputedHash(t *testing.T) {
	o, _ := supportPrepareFixture(t)
	files, err := prepareSupportFiles(o)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"legacy-support", "changed-profile"} {
		t.Run(name, func(t *testing.T) {
			var config deployment
			if err := json.Unmarshal(files["control.disabled.json"], &config); err != nil {
				t.Fatal(err)
			}
			switch name {
			case "legacy-support":
				config.Profiles[0].ProviderAuditPolicy = ""
				config.Profiles[0].ExpectedResponseModel = ""
				config.Profiles[0].ExecutorVersion = "legacy-v1"
			case "changed-profile":
				config.Profiles[0].FamilyCostMicroyuan++
			}
			if _, err := readDeployment(deploymentFile(t, string(supportJSON(config)))); err == nil {
				t.Fatal("invalid support capability accepted")
			}
		})
	}
}

func TestSupportSourceExampleMatchesReviewedFilesButRequiresReceipts(t *testing.T) {
	repo := filepath.Join("..", "..")
	raw, err := os.ReadFile(filepath.Join(repo, "deploy", "support-cloud.source.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	var source supportSource
	if err := json.Unmarshal(raw, &source); err != nil {
		t.Fatal(err)
	}
	profile, err := run.BuildSupportProfile("source-example-validation", source.Definition)
	if err != nil || profile.Executable || verifySupportSources(repo, source.Definition) != nil {
		t.Fatal("source example differs from the fixed definition or reviewed files", err)
	}
	if source.BuildReceipt.SHA256 != "" || source.DataReview.SHA256 != "" || source.ScoringReview.SHA256 != "" {
		t.Fatal("source example claims completed deployment/review receipts")
	}
}
