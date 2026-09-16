package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/xjfyrh/jobforge/internal/jsonstrict"
	"github.com/xjfyrh/jobforge/internal/run"
	"github.com/xjfyrh/jobforge/internal/runworker"
)

type supportReceipt struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type supportSource struct {
	SchemaVersion int                   `json:"schema_version"`
	Definition    run.SupportDefinition `json:"definition"`
	BuildReceipt  supportReceipt        `json:"build_receipt"`
	DataReview    supportReceipt        `json:"data_review"`
	ScoringReview supportReceipt        `json:"scoring_review"`
	PriceSnapshot supportReceipt        `json:"price_snapshot"`
}

type supportPrepareOptions struct {
	Source, Repo, ProfileID, WorkerID, BatchID, BatchKey, NorthID, SouthID, ValidFrom, Out string
	BatchCostMicroyuan                                                                     int64
}

type supportCase struct {
	Ordinal            int    `json:"ordinal"`
	CaseID             string `json:"case_id"`
	TenantID           string `json:"tenant_id"`
	TicketID           string `json:"ticket_id"`
	BusinessRequestKey string `json:"business_request_key"`
	IdempotencyKey     string `json:"idempotency_key"`
}

type supportLaunch struct {
	SchemaVersion        int               `json:"schema_version"`
	BatchAccountID       string            `json:"batch_account_id"`
	BatchKey             string            `json:"batch_key"`
	WorkerID             string            `json:"worker_id"`
	ProfileID            string            `json:"profile_id"`
	ProfileHash          string            `json:"profile_hash"`
	PriceHash            string            `json:"price_hash"`
	ValidFrom            time.Time         `json:"valid_from"`
	ValidUntil           time.Time         `json:"valid_until"`
	SourceManifestSHA256 string            `json:"source_manifest_sha256"`
	BuildReceiptSHA256   string            `json:"build_receipt_sha256"`
	DataReviewSHA256     string            `json:"data_review_sha256"`
	ScoringReviewSHA256  string            `json:"scoring_review_sha256"`
	ConfigSHA256         map[string]string `json:"config_sha256"`
	Cases                []supportCase     `json:"cases"`
}

func prepareSupport(args []string) error {
	var o supportPrepareOptions
	flags := flag.NewFlagSet("prepare-support", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.Int64Var(&o.BatchCostMicroyuan, "batch-cost-microyuan", 5000000, "shared batch cost limit within the remaining authorization")
	for name, target := range map[string]*string{"source": &o.Source, "repo": &o.Repo, "profile-id": &o.ProfileID,
		"worker-id": &o.WorkerID, "batch-id": &o.BatchID, "batch-key": &o.BatchKey,
		"north-account-id": &o.NorthID, "south-account-id": &o.SouthID, "valid-from": &o.ValidFrom, "out": &o.Out} {
		flags.StringVar(target, name, "", "required fixed batch input")
	}
	if flags.Parse(args) != nil || flags.NArg() != 0 {
		return errors.New("invalid prepare-support arguments")
	}
	files, err := prepareSupportFiles(o)
	if err != nil {
		return err
	}
	// Exclusive creation preserves prior rows and attempted state on every rerun.
	if os.Mkdir(o.Out, 0700) != nil {
		return errors.New("prepare output must be a new directory outside the repository")
	}
	for name, data := range files {
		if os.WriteFile(filepath.Join(o.Out, name), data, 0600) != nil {
			return errors.New("prepare output write failed; partial directory must not be launched")
		}
	}
	fmt.Println(`{"prepare_support":"complete","cases":40,"enabled":false}`)
	return nil
}

func prepareSupportFiles(o supportPrepareOptions) (map[string][]byte, error) {
	if o.BatchCostMicroyuan <= 0 || o.BatchCostMicroyuan > 5000000 {
		return nil, errors.New("batch cost limit must be between 1 and 5000000 microyuan")
	}
	from, err := time.Parse(time.RFC3339, o.ValidFrom)
	if err != nil || from.Format(time.RFC3339) != o.ValidFrom || !strings.HasSuffix(o.ValidFrom, "Z") ||
		!run.ValidIdentifier(o.ProfileID) || !run.ValidIdentifier(o.WorkerID) || !run.ValidIdentifier(o.BatchKey) ||
		!run.ValidUUID(o.BatchID) || !run.ValidUUID(o.NorthID) || !run.ValidUUID(o.SouthID) ||
		o.BatchID == o.NorthID || o.BatchID == o.SouthID || o.NorthID == o.SouthID ||
		o.Repo == "" || o.Source == "" || o.Out == "" || !outsideRepository(o.Repo, o.Out) {
		return nil, errors.New("prepare requires explicit identities, UTC validity and external output")
	}
	raw, err := readSupportFile(o.Source)
	var source supportSource
	if err != nil || jsonstrict.Decode(raw, &source) != nil || source.SchemaVersion != 1 {
		return nil, errors.New("invalid reviewed support source manifest")
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	d, err := run.DecodeSupportDefinition(fields["definition"])
	if err != nil {
		return nil, errors.New("invalid fixed support definition")
	}
	for _, receipt := range []supportReceipt{source.BuildReceipt, source.DataReview, source.ScoringReview, source.PriceSnapshot} {
		path := receipt.Path
		if !filepath.IsAbs(path) {
			path = filepath.Join(filepath.Dir(o.Source), path)
		}
		data, readErr := readSupportFile(path)
		if readErr != nil || !run.ValidHash(receipt.SHA256) || supportSHA256(data) != receipt.SHA256 {
			return nil, errors.New("reviewed support receipt digest mismatch")
		}
	}
	if source.PriceSnapshot.SHA256 != d.Price.SourceSHA256 || verifySupportSources(o.Repo, d) != nil {
		return nil, errors.New("reviewed support source digest mismatch")
	}
	profile, err := run.BuildSupportProfile(o.ProfileID, d)
	if err != nil {
		return nil, errors.New("fixed support profile could not be built")
	}
	cases, err := readSupportCases(o.Repo, o.BatchID)
	if err != nil {
		return nil, err
	}
	tenants := []string{"tenant-north", "tenant-south"}
	until := from.Add(6 * time.Hour)
	config := deployment{SchemaVersion: 1, Tenants: tenants, Profiles: []run.Profile{profile}, EnabledProfiles: []string{},
		Workers: []workerConfig{{ID: o.WorkerID, Tenants: tenants, Profiles: []string{o.ProfileID}, Capacity: 1}}, TenantCapacity: 1, ProfileCapacity: 1,
		Budgets: []budgetConfig{{ID: o.BatchID, Scope: "batch", Key: o.BatchKey, ValidFrom: from, ValidUntil: until, Limits: supportBudget(40)},
			{ID: o.NorthID, Scope: "tenant", Key: tenants[0], ValidFrom: from, ValidUntil: until, Limits: supportBudget(20)},
			{ID: o.SouthID, Scope: "tenant", Key: tenants[1], ValidFrom: from, ValidUntil: until, Limits: supportBudget(20)}},
		Bindings: []budgetBinding{{TenantID: tenants[0], BatchAccountID: o.BatchID, TenantAccountID: o.NorthID}, {TenantID: tenants[1], BatchAccountID: o.BatchID, TenantAccountID: o.SouthID}}}
	config.Budgets[0].Limits.CostMicroyuan = o.BatchCostMicroyuan
	files := map[string][]byte{}
	files["control.disabled.json"] = supportJSON(config)
	config.EnabledProfiles = []string{o.ProfileID}
	files["control.enabled.json"] = supportJSON(config)
	type endpoints struct {
		BusinessOrigin string `json:"business_origin"`
		OllamaOrigin   string `json:"ollama_origin"`
	}
	worker := struct {
		SchemaVersion int                  `json:"schema_version"`
		Profiles      []run.Profile        `json:"profiles"`
		Tenants       map[string]endpoints `json:"tenants"`
	}{
		1, []run.Profile{profile}, map[string]endpoints{tenants[0]: {"http://business:8092", "http://ollama:11434"}, tenants[1]: {"http://business:8092", "http://ollama:11434"}}}
	files["worker.json"] = supportJSON(worker)
	files["executor.json"] = supportJSON(runworker.Manifest{SchemaVersion: 1, ExecutorVersion: profile.ExecutorVersion,
		Profiles: []runworker.ManifestProfile{{ProfileID: profile.ID, ProfileHash: profile.Hash, AdapterID: "support-fixed-v1"}}})
	launch := supportLaunch{SchemaVersion: 1, BatchAccountID: o.BatchID, BatchKey: o.BatchKey, WorkerID: o.WorkerID, ProfileID: profile.ID,
		ProfileHash: profile.Hash, PriceHash: profile.Pricing.Hash, ValidFrom: from, ValidUntil: until, SourceManifestSHA256: supportSHA256(raw),
		BuildReceiptSHA256: source.BuildReceipt.SHA256, DataReviewSHA256: source.DataReview.SHA256, ScoringReviewSHA256: source.ScoringReview.SHA256,
		ConfigSHA256: map[string]string{}, Cases: cases}
	for name, data := range files {
		launch.ConfigSHA256[name] = supportSHA256(data)
	}
	files["launch.json"] = supportJSON(launch)
	var rows bytes.Buffer
	for _, row := range cases {
		data, _ := json.Marshal(struct {
			supportCase
			Status string `json:"status"`
		}{row, "unattempted"})
		rows.Write(data)
		rows.WriteByte('\n')
	}
	files["rows.jsonl"] = rows.Bytes()
	return files, nil
}

func supportBudget(families int64) run.Usage {
	return run.Usage{Chat: 12 * families, LogicalTools: 8 * families, QueryEmbedding: 8 * families,
		ProfileMetadataHTTP: 16 * families, BusinessToolHTTP: 8 * families, PhysicalHTTP: 44 * families,
		ProtocolCorrections: families, Tokens: 12595200 * families, CostMicroyuan: 5000000}
}

func outsideRepository(repo, output string) bool {
	root, err := filepath.Abs(repo)
	if err != nil {
		return false
	}
	target, err := filepath.Abs(output)
	if err != nil {
		return false
	}
	relative, err := filepath.Rel(root, target)
	return err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func readSupportFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 4<<20 {
		return nil, errors.New("support source must be a bounded regular file")
	}
	return io.ReadAll(io.LimitReader(f, (4<<20)+1))
}

func supportSHA256(raw []byte) string { hash := sha256.Sum256(raw); return hex.EncodeToString(hash[:]) }
func supportJSON(value any) []byte {
	data, _ := json.MarshalIndent(value, "", "  ")
	return append(data, '\n')
}

func verifySupportSources(repo string, d run.SupportDefinition) error {
	for name, want := range map[string]string{
		"api/support/v1/schema.json":                           d.Program.ProposalSchemaSHA256,
		"python/jobforge_agent/support_adapter.py":             d.Program.PromptSHA256,
		"examples/support-agent/runtime/dataset-manifest.json": d.Resources.RuntimeManifestSHA256,
		"examples/support-agent/runtime/seed.json":             d.Resources.SeedSHA256,
	} {
		data, err := readSupportFile(filepath.Join(repo, filepath.FromSlash(name)))
		if err != nil || supportSHA256(data) != want {
			return errors.New("source mismatch")
		}
	}
	got, err := supportAdapterSHA256(repo)
	if err != nil || got != d.Program.AdapterSourceSHA256 {
		return errors.New("adapter source mismatch")
	}
	return nil
}

// Source-set hash binds the complete installed Python package, with stable
// slash-separated relative paths and raw file digests, never executable paths.
func supportAdapterSHA256(repo string) (string, error) {
	paths, err := filepath.Glob(filepath.Join(repo, "python", "jobforge_agent", "*.py"))
	if err != nil || len(paths) == 0 {
		return "", errors.New("adapter sources unavailable")
	}
	slices.Sort(paths)
	parts := []string{"jobforge.support.adapter-source.v1"}
	for _, path := range paths {
		data, err := readSupportFile(path)
		if err != nil {
			return "", err
		}
		parts = append(parts, "python/jobforge_agent/"+filepath.Base(path), supportSHA256(data))
	}
	return run.Fingerprint(parts[0], parts[1:]...), nil
}

func readSupportCases(repo, batchID string) ([]supportCase, error) {
	raw, err := readSupportFile(filepath.Join(repo, "examples", "support-agent", "evaluation", "case-map.jsonl"))
	if err != nil || supportSHA256(raw) != "b07c6c56fe1cfc4a39b73c7f0063b055b6b9b4a43d0cb4ebc58dda295c698ce7" {
		return nil, errors.New("reviewed 40-case registration mismatch")
	}
	var cases []supportCase
	for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte{'\n'}) {
		var row struct {
			CaseID         string `json:"case_id"`
			TenantID       string `json:"tenant_id"`
			TicketID       string `json:"ticket_id"`
			AsOf           string `json:"as_of"`
			DatasetVersion string `json:"dataset_version"`
			PolicyVersion  string `json:"policy_version"`
		}
		if jsonstrict.Decode(line, &row) != nil {
			return nil, errors.New("invalid reviewed case registration")
		}
		cases = append(cases, supportCase{len(cases) + 1, row.CaseID, row.TenantID, row.TicketID, "support-" + batchID + "-" + row.CaseID, "submit-" + batchID + "-" + row.CaseID})
	}
	return cases, nil
}
