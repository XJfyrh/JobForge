package run

import (
	"encoding/hex"
	"encoding/json"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Subcall identifies one host-registered physical request, never a supplied URL.
type Subcall string

// Fixed physical capabilities retain separate authorization for each HTTP send.
const (
	SubcallGetOrder       Subcall = "get_order"
	SubcallGetDelivery    Subcall = "get_delivery"
	SubcallProfileVersion Subcall = "profile_version"
	SubcallProfileTags    Subcall = "profile_tags"
	SubcallQueryEmbedding Subcall = "query_embedding"
	SubcallSearchPolicy   Subcall = "search_policy"
	SubcallChat           Subcall = "chat"

	// QueryEmbeddingMaxInputTokens assumes the registered MiniLM tokenizer and
	// the fixed 512 UTF-8 byte query bound. Missing usage retains this full hold.
	QueryEmbeddingMaxInputTokens int64 = 512
	// UsageSettlementWindow is not shortened by session or Run termination.
	UsageSettlementWindow = 30 * 24 * time.Hour
)

// StepIdentity binds a command to the trusted cursor, input and frozen resources.
type StepIdentity struct {
	ID            string `json:"step_id"`
	Sequence      int64  `json:"sequence"`
	Kind          string `json:"kind"`
	CursorVersion int64  `json:"cursor_version"`
	InputHash     string `json:"input_hash"`
	ProfileID     string `json:"profile_id"`
	ProfileHash   string `json:"profile_hash"`
	SnapshotID    string `json:"snapshot_id"`
	SnapshotHash  string `json:"snapshot_hash"`
}

// CheckStep validates the expected cursor independently of execution authority.
func CheckStep(r Run, a Authority, step StepIdentity) error {
	if !ValidUUID(step.ID) || step.Sequence < 1 || step.Sequence > 32 || step.CursorVersion < 0 || step.CursorVersion >= 32 ||
		!ValidHash(step.InputHash) || !ValidHash(step.ProfileHash) || !ValidHash(step.SnapshotHash) ||
		!ValidIdentifier(step.ProfileID) || !ValidUUID(step.SnapshotID) || !validStepKind(step.Kind) {
		return ErrInvalidArgument
	}
	if step.ID != a.NextStepID || step.Kind != a.NextStepKind || step.Sequence != r.CursorVersion+1 ||
		step.CursorVersion != r.CursorVersion || step.InputHash != a.NextInputHash ||
		step.ProfileID != r.ProfileID || step.ProfileHash != r.ProfileHash ||
		step.SnapshotID != r.SnapshotID || step.SnapshotHash != r.SnapshotHash {
		return ErrStepConflict
	}
	return nil
}

func validStepKind(kind string) bool {
	switch kind {
	case "read_ticket", "get_order", "get_delivery", "search_policy", "model_proposal", "protocol_correction", "submit_proposal":
		return true
	default:
		return false
	}
}

// BeginToolRequest starts one logical invocation under current authority.
type BeginToolRequest struct {
	Lease            Lease
	Step             StepIdentity
	ToolInvocationID string
}

// BeginToolResponse grants a new invocation only on its first acceptance.
type BeginToolResponse struct {
	ToolInvocationID string
	NewlyStarted     bool
}

// ReserveCallRequest never accepts caller-computed price, tokens or endpoints.
type ReserveCallRequest struct {
	Lease            Lease
	Step             StepIdentity
	PhysicalCallID   string
	ToolInvocationID string
	Subcall          Subcall
	ParameterHash    string
	PriceHash        string
}

// CallBudget is a host-computed conservative exposure under an immutable tariff.
type CallBudget struct {
	InputTokens   int64 `json:"input_tokens"`
	OutputTokens  int64 `json:"output_tokens"`
	TotalTokens   int64 `json:"total_tokens"`
	CostMicroyuan int64 `json:"cost_microyuan"`
}

// CallReservation is an audit record, never permission to replay a network send.
type CallReservation struct {
	PhysicalCallID     string
	ToolInvocationID   string
	Subcall            Subcall
	ParameterHash      string
	PriceHash          string
	Ordinal            int64
	ReservedAt         time.Time
	DispatchExpiresAt  time.Time
	CallDeadline       time.Time
	Budget             CallBudget
	UsageKnown         bool
	MeasurementAnomaly bool
	ReportedUsage      *UsageReport
}

// ReserveCallResponse permits one send only when NewlyReserved is true.
type ReserveCallResponse struct {
	Reservation   CallReservation
	NewlyReserved bool
}

// UsageReport contains complete trusted metering and its canonical identity.
// Counts are nonnegative safe integers; reports above a reservation are retained
// as anomalies and freeze its accounts rather than being rejected or truncated.
type UsageReport struct {
	InputTokens       int64  `json:"input_tokens"`
	OutputTokens      int64  `json:"output_tokens"`
	CachedInputTokens int64  `json:"cached_input_tokens"`
	ReceiptHash       string `json:"receipt_hash"`
	UsageHash         string `json:"usage_hash"`
}

// Hash is computed from exact integers and the complete trusted receipt hash.
func (u UsageReport) Hash() string {
	return Fingerprint("jobforge.run.usage.v1", strconv.FormatInt(u.InputTokens, 10),
		strconv.FormatInt(u.OutputTokens, 10), strconv.FormatInt(u.CachedInputTokens, 10), u.ReceiptHash)
}

// Validate rejects incomplete or lossy metering before any account can change.
func (u UsageReport) Validate() error {
	if u.InputTokens < 0 || u.InputTokens > MaxSafeInteger || u.OutputTokens < 0 || u.OutputTokens > MaxSafeInteger ||
		u.CachedInputTokens < 0 || u.CachedInputTokens > u.InputTokens || !ValidHash(u.ReceiptHash) || u.UsageHash != u.Hash() {
		return ErrInvalidArgument
	}
	return nil
}

// ObserveCallRequest separates transport and business validity from metering.
type ObserveCallRequest struct {
	Lease            Lease
	Step             StepIdentity
	PhysicalCallID   string
	TransportOutcome string
	HTTPStatus       int
	ErrorCode        string
	BusinessOutcome  string
	UsageKnown       bool
	Usage            *UsageReport
}

// Validate rejects ambiguous transport, output and usage combinations.
func (r ObserveCallRequest) Validate() error {
	if !ValidUUID(r.PhysicalCallID) || r.UsageKnown != (r.Usage != nil) || !validObservationError(r.ErrorCode) {
		return ErrInvalidArgument
	}
	if r.TransportOutcome == "unknown" {
		if r.HTTPStatus != 0 || r.BusinessOutcome != "unknown" || r.Usage != nil {
			return ErrInvalidArgument
		}
	} else if r.TransportOutcome != "response" || r.HTTPStatus < 100 || r.HTTPStatus > 599 ||
		(r.BusinessOutcome != "accepted" && r.BusinessOutcome != "rejected") ||
		(r.BusinessOutcome == "accepted" && r.ErrorCode != "") {
		return ErrInvalidArgument
	}
	if r.Usage != nil {
		return r.Usage.Validate()
	}
	return nil
}

// Hash binds observation content, so an ACK retry cannot overwrite a prior fact.
func (r ObserveCallRequest) Hash() string {
	usageHash := ""
	if r.Usage != nil {
		usageHash = r.Usage.UsageHash
	}
	return Fingerprint("jobforge.run.observation.v1", r.TransportOutcome, strconv.Itoa(r.HTTPStatus),
		r.ErrorCode, r.BusinessOutcome, usageHash)
}

// SettleUsageRequest can only identify and settle an already reserved own call.
type SettleUsageRequest struct {
	Lease          Lease
	PhysicalCallID string
	Usage          UsageReport
}

// SettleUsageResponse distinguishes the first settlement from an identical retry.
type SettleUsageResponse struct {
	Reservation  CallReservation
	NewlySettled bool
}

// ValidHash checks the canonical lower-case representation shared by ledgers.
func ValidHash(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32 && strings.ToLower(value) == value
}

// ValidUUID rejects alternate spellings before they enter identity comparisons.
func ValidUUID(value string) bool {
	id, err := uuid.Parse(value)
	return err == nil && id.String() == value
}

// CallKind maps a fixed capability to its independently counted category.
func CallKind(subcall Subcall) string {
	switch subcall {
	case SubcallChat:
		return "chat"
	case SubcallQueryEmbedding:
		return "query_embedding"
	case SubcallProfileVersion, SubcallProfileTags:
		return "profile_metadata_http"
	case SubcallGetOrder, SubcallGetDelivery, SubcallSearchPolicy:
		return "business_tool_http"
	default:
		return ""
	}
}

// ToolSequence returns the registered ordered subcalls; callers receive a copy.
func ToolSequence(kind string) []Subcall {
	switch kind {
	case "get_order":
		return []Subcall{SubcallGetOrder}
	case "get_delivery":
		return []Subcall{SubcallGetDelivery}
	case "search_policy":
		return []Subcall{SubcallProfileVersion, SubcallProfileTags, SubcallQueryEmbedding, SubcallSearchPolicy}
	default:
		return nil
	}
}

// ReservationBudget derives bounds entirely from registered execution resources.
func ReservationBudget(profile Profile, subcall Subcall) (CallBudget, error) {
	switch CallKind(subcall) {
	case "chat":
		if profile.MaxInputTokens < 1 || profile.MaxInputTokens > MaxSafeInteger || profile.MaxOutputTokens < 1 ||
			profile.MaxOutputTokens > 1024 || profile.MaxInputTokens > MaxSafeInteger-profile.MaxOutputTokens {
			return CallBudget{}, ErrProfileUnavailable
		}
		upper := UsageReport{InputTokens: profile.MaxInputTokens, OutputTokens: profile.MaxOutputTokens}
		if profile.Pricing.InputHitMicroyuan > profile.Pricing.InputMissMicroyuan {
			upper.CachedInputTokens = upper.InputTokens
		}
		cost, err := UsageCost(profile.Pricing, upper)
		if err != nil {
			return CallBudget{}, err
		}
		return CallBudget{InputTokens: upper.InputTokens, OutputTokens: upper.OutputTokens,
			TotalTokens: upper.InputTokens + upper.OutputTokens, CostMicroyuan: cost}, nil
	case "query_embedding":
		return CallBudget{InputTokens: QueryEmbeddingMaxInputTokens, TotalTokens: QueryEmbeddingMaxInputTokens}, nil
	case "profile_metadata_http", "business_tool_http":
		return CallBudget{}, nil
	default:
		return CallBudget{}, ErrInvalidArgument
	}
}

// UsageCost uses exact rational arithmetic and rounds the complete amount up.
// It never overflows intermediate multiplication or converts money to float.
func UsageCost(price Pricing, usage UsageReport) (int64, error) {
	for _, value := range []int64{price.Denominator, price.InputMissMicroyuan, price.InputHitMicroyuan, price.OutputMicroyuan,
		usage.InputTokens, usage.OutputTokens, usage.CachedInputTokens} {
		if value < 0 || value > MaxSafeInteger {
			return 0, ErrInvalidArgument
		}
	}
	if price.Denominator == 0 || usage.CachedInputTokens > usage.InputTokens {
		return 0, ErrInvalidArgument
	}
	numerator := new(big.Int)
	for _, term := range [][2]int64{{usage.InputTokens - usage.CachedInputTokens, price.InputMissMicroyuan},
		{usage.CachedInputTokens, price.InputHitMicroyuan}, {usage.OutputTokens, price.OutputMicroyuan}} {
		numerator.Add(numerator, new(big.Int).Mul(big.NewInt(term[0]), big.NewInt(term[1])))
	}
	denominator := big.NewInt(price.Denominator)
	numerator.Add(numerator, new(big.Int).Sub(denominator, big.NewInt(1)))
	numerator.Quo(numerator, denominator)
	if !numerator.IsInt64() || numerator.Int64() > MaxSafeInteger {
		return 0, ErrBudgetExhausted
	}
	return numerator.Int64(), nil
}

// ReservationUsage includes irrevocable category counts and conservative holds.
func ReservationUsage(stepKind string, subcall Subcall, budget CallBudget) Usage {
	u := Usage{PhysicalHTTP: 1, Tokens: budget.TotalTokens, CostMicroyuan: budget.CostMicroyuan}
	switch CallKind(subcall) {
	case "chat":
		u.Chat = 1
		if stepKind == "protocol_correction" {
			u.ProtocolCorrections = 1
		}
	case "query_embedding":
		u.QueryEmbedding = 1
	case "profile_metadata_http":
		u.ProfileMetadataHTTP = 1
	case "business_tool_http":
		u.BusinessToolHTTP = 1
	}
	return u
}

// UsageJSON encodes only the validated typed report, not arbitrary caller JSON.
func UsageJSON(usage UsageReport) ([]byte, error) {
	if err := usage.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(usage)
}

func validObservationError(code string) bool {
	switch code {
	case "", "TIMEOUT", "DEPENDENCY_UNAVAILABLE", "INVALID_ARGUMENT", "PROFILE_UNAVAILABLE", "BUDGET_EXHAUSTED",
		"MODEL_PROTOCOL_ERROR", "MODEL_UNSUPPORTED", "EXECUTOR_PROTOCOL_ERROR", "CHECKPOINT_TOO_LARGE", "HTTP_ERROR":
		return true
	default:
		return false
	}
}
