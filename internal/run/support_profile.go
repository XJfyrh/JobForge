package run

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"

	"github.com/xjfyrh/jobforge/internal/business"
	"github.com/xjfyrh/jobforge/internal/jsonstrict"
)

const (
	// SupportDefinitionMaxBytes is the complete immutable definition bound.
	SupportDefinitionMaxBytes = 16 * 1024
	// SupportDatasetID identifies the reviewed first-batch dataset.
	SupportDatasetID = "support-dev-2026-09-16-v2"
	// SupportPolicyVersion identifies the reviewed first-batch policy.
	SupportPolicyVersion = "delivery-policy-dev-v2"
	// SupportObservedAt fixes the business observation instant.
	SupportObservedAt = "2026-09-16T12:00:00Z"
	// SupportPromptVersion identifies the reviewed fixed prompt source contract.
	SupportPromptVersion = "support-fixed-prompt-v1"
)

// SupportDefinition contains only the reviewed first-batch capability and resources.
// Source digest correspondence is verified by preparation/deployment, not by admission.
type SupportDefinition struct {
	SchemaVersion int                       `json:"schema_version"`
	Model         SupportModelDefinition    `json:"model"`
	Program       SupportProgramDefinition  `json:"program"`
	Resources     SupportResourceDefinition `json:"resources"`
	Price         SupportPriceDefinition    `json:"price"`
}

// SupportModelDefinition fixes the single allowed physical provider request.
type SupportModelDefinition struct {
	Provider            string `json:"provider"`
	Origin              string `json:"origin"`
	Path                string `json:"path"`
	RequestModel        string `json:"request_model"`
	ObservedVersion     string `json:"observed_version"`
	ObservedOn          string `json:"observed_on"`
	Stream              bool   `json:"stream"`
	Thinking            string `json:"thinking"`
	MaxTokens           int64  `json:"max_tokens"`
	ResponseFormat      string `json:"response_format"`
	Temperature         int64  `json:"temperature"`
	RequestBodyBytes    int64  `json:"request_body_bytes"`
	MessageContentBytes int64  `json:"message_content_bytes"`
	ResponseBodyBytes   int64  `json:"response_body_bytes"`
	ModelContentBytes   int64  `json:"model_content_bytes"`
}

// SupportProgramDefinition binds reviewed code artifacts without executable paths.
type SupportProgramDefinition struct {
	Strategy             string `json:"strategy"`
	Adapter              string `json:"adapter"`
	ProposalSchema       string `json:"proposal_schema"`
	ProposalSchemaSHA256 string `json:"proposal_schema_sha256"`
	PromptVersion        string `json:"prompt_version"`
	PromptSHA256         string `json:"prompt_sha256"`
	AdapterSourceSHA256  string `json:"adapter_source_sha256"`
}

// SupportResourceDefinition freezes the resources expressible by existing snapshots.
type SupportResourceDefinition struct {
	DatasetID             string                    `json:"dataset_id"`
	RuntimeManifestSHA256 string                    `json:"runtime_manifest_sha256"`
	SeedSHA256            string                    `json:"seed_sha256"`
	ObservedAt            string                    `json:"observed_at"`
	PolicyVersion         string                    `json:"policy_version"`
	CorpusSHA256          string                    `json:"corpus_sha256"`
	IndexProfile          business.IndexProfile     `json:"index_profile"`
	IndexProfileHash      string                    `json:"index_profile_hash"`
	Tenants               []SupportTenantDefinition `json:"tenants"`
}

// SupportTenantDefinition selects one immutable published tenant index.
type SupportTenantDefinition struct {
	TenantID         string `json:"tenant_id"`
	PolicyRevision   int64  `json:"policy_revision"`
	IndexID          string `json:"index_id"`
	IndexContentHash string `json:"index_content_hash"`
}

// SupportPriceDefinition preserves the accepted official tariff snapshot.
type SupportPriceDefinition struct {
	ObservedOn         string `json:"observed_on"`
	SourceSHA256       string `json:"source_sha256"`
	Currency           string `json:"currency"`
	Denominator        int64  `json:"denominator"`
	InputMissMicroyuan int64  `json:"input_miss_microyuan"`
	InputHitMicroyuan  int64  `json:"input_hit_microyuan"`
	OutputMicroyuan    int64  `json:"output_microyuan"`
}

// DecodeSupportDefinition rejects incomplete, ambiguous or non-fixed definitions.
func DecodeSupportDefinition(raw []byte) (SupportDefinition, error) {
	var d SupportDefinition
	if len(raw) > SupportDefinitionMaxBytes || jsonstrict.Decode(raw, &d) != nil {
		return d, ErrProfileUnavailable
	}
	canonical, err := CanonicalSupportDefinition(d)
	if err != nil {
		return d, err
	}
	actual, err := supportCanonical(raw)
	// Typed reconstruction includes every field, including false/zero: missing
	// fields cannot acquire their Go zero value and silently become accepted.
	if err != nil || !bytes.Equal(actual, canonical) {
		return d, ErrProfileUnavailable
	}
	return d, nil
}

// CanonicalSupportDefinition retains exact integers and uses Go JSON string escaping.
func CanonicalSupportDefinition(d SupportDefinition) (json.RawMessage, error) {
	if d.validate() != nil {
		return nil, ErrProfileUnavailable
	}
	raw, err := json.Marshal(d)
	if err != nil || len(raw) > SupportDefinitionMaxBytes {
		return nil, ErrProfileUnavailable
	}
	return supportCanonical(raw)
}

func supportCanonical(raw []byte) (json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, ErrProfileUnavailable
	}
	return json.Marshal(value)
}

func supportDigest(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && hex.EncodeToString(decoded) == value
}

func (d SupportDefinition) validate() error {
	m, p, r, price := d.Model, d.Program, d.Resources, d.Price
	if d.SchemaVersion != 1 || m != (SupportModelDefinition{
		Provider: "deepseek", Origin: "https://api.deepseek.com", Path: "/chat/completions",
		RequestModel: "deepseek-flash", ObservedVersion: "DeepSeek-V4.1-Flash", ObservedOn: "2026-09-16",
		Thinking: "disabled", MaxTokens: 1024, ResponseFormat: "json_object",
		RequestBodyBytes: 65536, MessageContentBytes: 16384, ResponseBodyBytes: 65536, ModelContentBytes: 16384,
	}) || p.Strategy != SupportFixedStrategy || p.Adapter != "support-fixed-v1" ||
		p.ProposalSchema != SupportProposalSchema || p.PromptVersion != SupportPromptVersion ||
		r.DatasetID != SupportDatasetID || r.PolicyVersion != SupportPolicyVersion || r.ObservedAt != SupportObservedAt ||
		price.ObservedOn != "2026-09-16" || price.Currency != "CNY" || price.Denominator != 1000000 ||
		price.InputMissMicroyuan != 2000000 || price.InputHitMicroyuan != 40000 || price.OutputMicroyuan != 8000000 {
		return ErrProfileUnavailable
	}
	for _, hash := range []string{p.ProposalSchemaSHA256, p.PromptSHA256, p.AdapterSourceSHA256,
		r.RuntimeManifestSHA256, r.SeedSHA256, r.CorpusSHA256, r.IndexProfileHash, price.SourceSHA256} {
		if !supportDigest(hash) {
			return ErrProfileUnavailable
		}
	}
	if r.IndexProfile != (business.IndexProfile{PolicyVersion: r.PolicyVersion, CorpusSHA256: r.CorpusSHA256,
		ChunkerVersion: business.ChunkerVersion, EmbeddingModel: business.EmbeddingModel,
		EmbeddingDigest: business.EmbeddingDigest, Dimensions: business.Dimensions}) || len(r.Tenants) != 2 {
		return ErrProfileUnavailable
	}
	encoded, _ := json.Marshal(r.IndexProfile) // Identical to the business index/capture contract.
	digest := sha256.Sum256(encoded)
	if hex.EncodeToString(digest[:]) != r.IndexProfileHash {
		return ErrProfileUnavailable
	}
	for i, tenant := range r.Tenants {
		if tenant.TenantID != []string{"tenant-north", "tenant-south"}[i] || tenant.PolicyRevision != 2 ||
			!ValidUUID(tenant.IndexID) || !supportDigest(tenant.IndexContentHash) {
			return ErrProfileUnavailable
		}
	}
	return nil
}

// SupportPriceHash computes the accepted tariff identity, excluding generated metadata.
func SupportPriceHash(d SupportDefinition) (string, error) {
	if d.validate() != nil {
		return "", ErrProfileUnavailable
	}
	p := d.Price
	return Fingerprint("jobforge.run.support-price.v1", d.Model.Provider, d.Model.RequestModel,
		d.Model.ObservedVersion, p.ObservedOn, p.SourceSHA256, p.Currency, decimal(p.Denominator),
		decimal(p.InputMissMicroyuan), decimal(p.InputHitMicroyuan), decimal(p.OutputMicroyuan)), nil
}

// BuildSupportProfile creates the one first-batch capability, disabled by default.
func BuildSupportProfile(id string, definition SupportDefinition) (Profile, error) {
	raw, err := CanonicalSupportDefinition(definition)
	if err != nil {
		return Profile{}, err
	}
	priceHash, err := SupportPriceHash(definition)
	if err != nil {
		return Profile{}, err
	}
	d := definition.Price
	p := Profile{ID: id, Strategy: SupportFixedStrategy, ExecutorVersion: ProviderAuditExecutorVersion,
		ProviderAuditPolicy: ProviderAuditPolicyDeepSeekV1, ExpectedResponseModel: "deepseek-flash",
		MaxInputTokens: 1048576, MaxOutputTokens: 1024, FamilyTokenLimit: 12595200, FamilyCostMicroyuan: 5000000,
		Pricing: Pricing{Hash: priceHash, Denominator: d.Denominator, InputMissMicroyuan: d.InputMissMicroyuan,
			InputHitMicroyuan: d.InputHitMicroyuan, OutputMicroyuan: d.OutputMicroyuan}, Definition: raw}
	p.Hash, err = SupportProfileHash(p)
	return p, err
}

// SupportProfileHash validates duplicated facts before computing the entire frozen identity.
func SupportProfileHash(p Profile) (string, error) {
	d, err := DecodeSupportDefinition(p.Definition)
	if err != nil || !ValidIdentifier(p.ID) || p.Strategy != SupportFixedStrategy ||
		p.ExecutorVersion != ProviderAuditExecutorVersion || p.ProviderAuditPolicy != ProviderAuditPolicyDeepSeekV1 ||
		p.ExpectedResponseModel != "deepseek-flash" || p.MaxInputTokens != 1048576 || p.MaxOutputTokens != 1024 ||
		p.FamilyTokenLimit != 12595200 || p.FamilyCostMicroyuan != 5000000 {
		return "", ErrProfileUnavailable
	}
	priceHash, _ := SupportPriceHash(d)
	if p.Pricing != (Pricing{Hash: priceHash, Denominator: d.Price.Denominator, InputMissMicroyuan: d.Price.InputMissMicroyuan,
		InputHitMicroyuan: d.Price.InputHitMicroyuan, OutputMicroyuan: d.Price.OutputMicroyuan}) {
		return "", ErrProfileUnavailable
	}
	canonical, _ := CanonicalSupportDefinition(d)
	return Fingerprint("jobforge.run.support-profile.v1", "1", p.ID, p.Strategy, p.ExecutorVersion,
		p.ProviderAuditPolicy, p.ExpectedResponseModel, decimal(p.MaxInputTokens), decimal(p.MaxOutputTokens),
		decimal(p.FamilyTokenLimit), decimal(p.FamilyCostMicroyuan), p.Pricing.Hash, decimal(p.Pricing.Denominator),
		decimal(p.Pricing.InputMissMicroyuan), decimal(p.Pricing.InputHitMicroyuan), decimal(p.Pricing.OutputMicroyuan), string(canonical)), nil
}

// ValidateSupportProfile applies only to the new audited support capability;
// historical definitions remain readable for replay and late accounting.
func ValidateSupportProfile(p Profile) error {
	if p.Strategy != SupportFixedStrategy || !p.AuditEnabled() {
		return nil
	}
	hash, err := SupportProfileHash(p)
	if err != nil || hash != p.Hash {
		return ErrProfileUnavailable
	}
	return nil
}

func decimal(value int64) string { return strconv.FormatInt(value, 10) }
