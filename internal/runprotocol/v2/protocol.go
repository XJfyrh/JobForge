// Package runprotocol validates the bounded executor wire protocol from
// api/executor/v2/schema.json. It grants no lease, network, or process authority.
package runprotocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"regexp"
	"strconv"
	"unicode/utf8"

	"github.com/xjfyrh/jobforge/internal/run"
)

const (
	// MaxFrameBytes includes the terminating LF of one UTF-8 JSON Lines frame.
	MaxFrameBytes = 384 * 1024
	// MaxMeteringFrameBytes bounds the dedicated metering FD including LF.
	MaxMeteringFrameBytes = 8 * 1024
	// MaxCheckpointBytes bounds the complete protected checkpoint object.
	MaxCheckpointBytes = 256 * 1024
	// MaxInteger is the largest exactly representable integer in the wire contract.
	MaxInteger int64 = 1<<53 - 1
)

var (
	// ErrProtocol is deliberately content-free: never log rejected frame bodies.
	ErrProtocol = errors.New("invalid executor protocol")
	// ErrFrameLimit identifies an oversized frame before parsing its content.
	ErrFrameLimit = errors.New("executor frame exceeds limit")
	uuidPattern   = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	keyPattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	hashPattern   = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Binding carries immutable request, execution, resource and cursor identities.
type Binding struct {
	TenantID      string `json:"tenant_id"`
	WorkerID      string `json:"worker_id"`
	RunID         string `json:"run_id"`
	StepID        string `json:"step_id"`
	SessionID     string `json:"session_id"`
	ProfileID     string `json:"profile_id"`
	ProfileHash   string `json:"profile_hash"`
	SnapshotID    string `json:"snapshot_id"`
	SnapshotHash  string `json:"snapshot_hash"`
	InputHash     string `json:"input_hash"`
	AttemptNo     int64  `json:"attempt_no"`
	FencingToken  int64  `json:"fencing_token"`
	CursorVersion int64  `json:"cursor_version"`
	StepSequence  int64  `json:"step_sequence"`
	StepKind      string `json:"step_kind"`
}

// Usage is trusted complete metering; it contains no caller-selected price.
type Usage struct {
	InputTokens       int64  `json:"input_tokens"`
	OutputTokens      int64  `json:"output_tokens"`
	CachedInputTokens int64  `json:"cached_input_tokens"`
	ReceiptHash       string `json:"receipt_hash"`
	UsageHash         string `json:"usage_hash"`
}

// Frame is an explicitly discriminated union. Decode requires exactly the fields
// defined for Kind; unused fields cannot be smuggled through zero values.
type Frame struct {
	EmittedMonoMS    int64              `json:"emitted_mono_ms"`
	Version          int64              `json:"version"`
	Kind             string             `json:"kind"`
	RequestID        string             `json:"request_id"`
	Binding          Binding            `json:"binding"`
	RemainingMS      int64              `json:"remaining_ms"`
	TraceContext     string             `json:"trace_context"`
	Checkpoint       json.RawMessage    `json:"checkpoint"`
	Input            json.RawMessage    `json:"input"`
	CallSequence     int64              `json:"call_sequence"`
	Subcall          string             `json:"subcall"`
	ParameterHash    string             `json:"parameter_hash"`
	ToolInvocationID string             `json:"tool_invocation_id"`
	PhysicalCallID   string             `json:"physical_call_id"`
	Granted          bool               `json:"granted"`
	ErrorCode        string             `json:"error_code"`
	DispatchMS       int64              `json:"dispatch_ms"`
	CallMS           int64              `json:"call_ms"`
	InputTokenLimit  int64              `json:"input_token_limit"`
	OutputTokenLimit int64              `json:"output_token_limit"`
	TransportOutcome string             `json:"transport_outcome"`
	HTTPStatus       int64              `json:"http_status"`
	BusinessOutcome  string             `json:"business_outcome"`
	UsageDisposition string             `json:"usage_disposition"`
	UsageHash        *string            `json:"usage_hash"`
	AuditHash        *string            `json:"audit_hash"`
	ProviderAudit    *run.ProviderAudit `json:"provider_audit"`
	ReportHash       string             `json:"report_hash"`
	ObservationHash  string             `json:"observation_hash"`
	Settlement       string             `json:"settlement"`
	Usage            *Usage             `json:"usage"`
	Outcome          string             `json:"outcome"`
	Result           json.RawMessage    `json:"result"`
}

var commonFields = []string{"version", "kind", "request_id", "binding", "emitted_mono_ms"}

var kindFields = map[string][]string{
	"execute_step":         {"remaining_ms", "trace_context", "checkpoint", "input"},
	"call_intent":          {"call_sequence", "subcall", "parameter_hash", "tool_invocation_id"},
	"call_permit":          {"call_sequence", "subcall", "parameter_hash", "tool_invocation_id", "physical_call_id", "granted", "error_code", "dispatch_ms", "call_ms", "input_token_limit", "output_token_limit"},
	"call_observation":     {"call_sequence", "physical_call_id", "transport_outcome", "http_status", "business_outcome", "error_code", "usage_disposition", "usage_hash", "audit_hash"},
	"call_observation_ack": {"call_sequence", "physical_call_id", "observation_hash"},
	"step_result":          {"outcome", "error_code", "result"},
	"metering_report":      {"call_sequence", "physical_call_id", "parameter_hash", "usage", "provider_audit", "report_hash"},
	"metering_ack":         {"call_sequence", "physical_call_id", "report_hash", "settlement"},
}

var bindingFields = []string{"tenant_id", "worker_id", "run_id", "step_id", "session_id", "profile_id", "profile_hash", "snapshot_id", "snapshot_hash", "input_hash", "attempt_no", "fencing_token", "cursor_version", "step_sequence", "step_kind"}
var usageFields = []string{"input_tokens", "output_tokens", "cached_input_tokens", "receipt_hash", "usage_hash"}

// Decode validates exactly one complete LF-terminated frame with no trailing stdout.
func Decode(line []byte) (Frame, error) {
	var frame Frame
	if len(line) > MaxFrameBytes {
		return frame, ErrFrameLimit
	}
	if len(line) == 0 || line[len(line)-1] != '\n' || !utf8.Valid(line) || bytes.ContainsAny(line[:len(line)-1], "\r\n") {
		return frame, ErrProtocol
	}
	body := line[:len(line)-1]
	if !strictJSON(body) {
		return frame, ErrProtocol
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal(body, &raw) != nil || raw == nil || json.Unmarshal(body, &frame) != nil {
		return Frame{}, ErrProtocol
	}
	fields, ok := kindFields[frame.Kind]
	if !ok || !exactFields(raw, append(append([]string{}, commonFields...), fields...), "usage_hash", "audit_hash", "usage", "provider_audit") ||
		!objectFields(raw["binding"], bindingFields) || frame.Version != 2 || !between(frame.EmittedMonoMS, 0, MaxInteger) ||
		!uuidPattern.MatchString(frame.RequestID) || !validBinding(frame.Binding) {
		return Frame{}, ErrProtocol
	}
	if oneOf(frame.Kind, "metering_report", "metering_ack") && len(line) > MaxMeteringFrameBytes {
		return Frame{}, ErrFrameLimit
	}
	if err := validateFrame(frame); err != nil {
		return Frame{}, err
	}
	if frame.Usage != nil && !objectFields(raw["usage"], usageFields) {
		return Frame{}, ErrProtocol
	}
	return frame, nil
}

// Encode selects exactly the fields for Kind, then applies the same strict codec.
func Encode(frame Frame) ([]byte, error) {
	fields, ok := kindFields[frame.Kind]
	if !ok {
		return nil, ErrProtocol
	}
	all, err := json.Marshal(frame)
	if err != nil {
		return nil, ErrProtocol
	}
	var raw map[string]json.RawMessage
	if err = json.Unmarshal(all, &raw); err != nil {
		return nil, ErrProtocol
	}
	selected := make(map[string]json.RawMessage, len(fields)+len(commonFields))
	for _, key := range append(append([]string{}, commonFields...), fields...) {
		selected[key] = raw[key]
	}
	body, err := json.Marshal(selected)
	if err != nil {
		return nil, ErrProtocol
	}
	line := append(body, '\n')
	if _, err = Decode(line); err != nil {
		return nil, err
	}
	return line, nil
}

// ReadFrame bounds allocation before accepting a newline; the caller owns I/O
// deadlines and cancellation. It consumes no byte from the following frame.
func ReadFrame(reader io.Reader) (Frame, error) {
	return readBoundedFrame(reader, MaxFrameBytes, DecodeOrdinary)
}

// ReadMeteringFrame enforces the dedicated pipe's limit before allocating a body.
func ReadMeteringFrame(reader io.Reader) (Frame, error) {
	return readBoundedFrame(reader, MaxMeteringFrameBytes, DecodeMetering)
}

func readBoundedFrame(reader io.Reader, limit int, decode func([]byte) (Frame, error)) (Frame, error) {
	line := make([]byte, 0, 4096)
	one := make([]byte, 1)
	for len(line) < limit {
		n, err := reader.Read(one)
		if n == 1 {
			line = append(line, one[0])
			if one[0] == '\n' {
				return decode(line)
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) && len(line) > 0 {
				return Frame{}, ErrProtocol
			}
			return Frame{}, err
		}
		if n == 0 {
			return Frame{}, io.ErrNoProgress
		}
	}
	return Frame{}, ErrFrameLimit
}

// DecodeOrdinary rejects metering frames on the execution pipe.
func DecodeOrdinary(line []byte) (Frame, error) {
	f, err := Decode(line)
	if err == nil && oneOf(f.Kind, "metering_report", "metering_ack") {
		return Frame{}, ErrProtocol
	}
	return f, err
}

// DecodeMetering rejects ordinary frames and bounds input before JSON parsing.
func DecodeMetering(line []byte) (Frame, error) {
	if len(line) > MaxMeteringFrameBytes {
		return Frame{}, ErrFrameLimit
	}
	f, err := Decode(line)
	if err == nil && !oneOf(f.Kind, "metering_report", "metering_ack") {
		return Frame{}, ErrProtocol
	}
	return f, err
}

// EncodeOrdinary selects an execution-pipe frame only.
func EncodeOrdinary(frame Frame) ([]byte, error) {
	if oneOf(frame.Kind, "metering_report", "metering_ack") {
		return nil, ErrProtocol
	}
	return Encode(frame)
}

// EncodeMetering selects a dedicated-FD frame only.
func EncodeMetering(frame Frame) ([]byte, error) {
	if !oneOf(frame.Kind, "metering_report", "metering_ack") {
		return nil, ErrProtocol
	}
	return Encode(frame)
}

func validBinding(b Binding) bool {
	return keyPattern.MatchString(b.TenantID) && keyPattern.MatchString(b.WorkerID) && keyPattern.MatchString(b.ProfileID) &&
		uuidPattern.MatchString(b.RunID) && uuidPattern.MatchString(b.StepID) && uuidPattern.MatchString(b.SessionID) && uuidPattern.MatchString(b.SnapshotID) &&
		hashPattern.MatchString(b.ProfileHash) && hashPattern.MatchString(b.SnapshotHash) && hashPattern.MatchString(b.InputHash) &&
		between(b.AttemptNo, 1, MaxInteger) && between(b.FencingToken, 1, MaxInteger) && between(b.CursorVersion, 0, MaxInteger) &&
		between(b.StepSequence, 1, 32) && oneOf(b.StepKind, "read_ticket", "get_order", "get_delivery", "search_policy", "model_proposal", "protocol_correction", "submit_proposal")
}

func validateFrame(f Frame) error {
	valid := false
	switch f.Kind {
	case "execute_step":
		if len(f.Checkpoint) > MaxCheckpointBytes || len(f.Input) > 16384 {
			return ErrFrameLimit
		}
		valid = between(f.RemainingMS, 1, 180000) && validTrace(f.TraceContext) && boundedObject(f.Checkpoint, MaxCheckpointBytes) && boundedObject(f.Input, 16384)
	case "call_intent", "call_permit":
		valid = between(f.CallSequence, 1, 44) && registeredSubcall(f.Binding.StepKind, f.Subcall) && hashPattern.MatchString(f.ParameterHash) &&
			((f.Subcall == "chat" && f.ToolInvocationID == "") || (f.Subcall != "chat" && uuidPattern.MatchString(f.ToolInvocationID)))
		if f.Kind == "call_permit" {
			valid = valid && validPermit(f)
		}
	case "call_observation":
		_, errorCodeErr := ObservationErrorCode(f.ErrorCode)
		valid = between(f.CallSequence, 1, 44) && uuidPattern.MatchString(f.PhysicalCallID) && validDisposition(f) && validAuditHash(f) && errorCodeErr == nil &&
			((f.TransportOutcome == "response" && between(f.HTTPStatus, 100, 599) && oneOf(f.BusinessOutcome, "accepted", "rejected")) ||
				(f.TransportOutcome == "unknown" && f.HTTPStatus == 0 && f.BusinessOutcome == "unknown" && f.UsageDisposition == "unknown")) &&
			((f.BusinessOutcome == "accepted" && f.ErrorCode == "") || (f.BusinessOutcome != "accepted" && f.ErrorCode != ""))
	case "call_observation_ack":
		valid = between(f.CallSequence, 1, 44) && uuidPattern.MatchString(f.PhysicalCallID) && hashPattern.MatchString(f.ObservationHash)
	case "metering_report":
		valid = between(f.CallSequence, 1, 44) && uuidPattern.MatchString(f.PhysicalCallID) && hashPattern.MatchString(f.ParameterHash) && validReport(f)
	case "metering_ack":
		valid = between(f.CallSequence, 1, 44) && uuidPattern.MatchString(f.PhysicalCallID) && hashPattern.MatchString(f.ReportHash) && oneOf(f.Settlement, "settled", "recorded", "anomaly", "conflict", "unconfirmed")
	case "step_result":
		limit := 16384
		if oneOf(f.Binding.StepKind, "read_ticket", "get_order", "get_delivery", "search_policy") {
			limit = 8192
		}
		if len(f.Result) > limit {
			return ErrFrameLimit
		}
		valid = boundedObject(f.Result, limit) && validError(f.ErrorCode) &&
			((f.Outcome == "success" && f.ErrorCode == "") || (f.Outcome == "error" && f.ErrorCode != ""))
	}
	if !valid {
		return ErrProtocol
	}
	return nil
}

func validPermit(f Frame) bool {
	if !validError(f.ErrorCode) || !between(f.InputTokenLimit, 0, MaxInteger) || !between(f.OutputTokenLimit, 0, 1024) || f.InputTokenLimit > MaxInteger-f.OutputTokenLimit {
		return false
	}
	if !f.Granted {
		return f.PhysicalCallID == "" && f.ErrorCode != "" && f.DispatchMS == 0 && f.CallMS == 0 && f.InputTokenLimit == 0 && f.OutputTokenLimit == 0
	}
	maxCall := int64(10000)
	if oneOf(f.Subcall, "chat", "query_embedding") {
		maxCall = 60000
	} else if f.InputTokenLimit != 0 || f.OutputTokenLimit != 0 {
		return false
	}
	return uuidPattern.MatchString(f.PhysicalCallID) && f.ErrorCode == "" && between(f.DispatchMS, 1, 30000) && between(f.CallMS, 1, maxCall) && f.DispatchMS <= f.CallMS
}

func registeredSubcall(step, subcall string) bool {
	switch step {
	case "get_order", "get_delivery":
		return step == subcall
	case "search_policy":
		return oneOf(subcall, "profile_version", "profile_tags", "query_embedding", "search_policy")
	case "model_proposal", "protocol_correction":
		return subcall == "chat"
	default:
		return false
	}
}

func validError(code string) bool {
	return oneOf(code, "", "BUDGET_EXHAUSTED", "STOP_REQUESTED", "STALE_LEASE", "PROFILE_UNAVAILABLE", "DEPENDENCY_UNAVAILABLE", "PROTOCOL_ERROR", "CALL_CONFLICT", "OUTPUT_INVALID", "INPUT_INVALID", "TIMEOUT")
}

func validTrace(trace string) bool {
	if len(trace) > 512 {
		return false
	}
	for _, char := range trace {
		if char < 0x20 || char > 0x7e {
			return false
		}
	}
	return true
}

func boundedObject(raw json.RawMessage, limit int) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(raw) <= limit && len(trimmed) >= 2 && trimmed[0] == '{'
}

func between(value, low, high int64) bool { return value >= low && value <= high }

func oneOf(value string, choices ...string) bool {
	for _, choice := range choices {
		if value == choice {
			return true
		}
	}
	return false
}

func exactFields(raw map[string]json.RawMessage, fields []string, nullable ...string) bool {
	if len(raw) != len(fields) {
		return false
	}
	for _, field := range fields {
		value, ok := raw[field]
		if !ok || (!oneOf(field, nullable...) && bytes.Equal(bytes.TrimSpace(value), []byte("null"))) {
			return false
		}
	}
	return true
}

func objectFields(data []byte, fields []string) bool {
	var raw map[string]json.RawMessage
	return json.Unmarshal(data, &raw) == nil && raw != nil && exactFields(raw, fields, "")
}

// strictJSON rejects duplicate keys recursively, non-finite numbers, trailing
// values and nesting beyond 64 before decoding into typed protocol structures.
func strictJSON(data []byte) bool {
	if !validEscapes(data) {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if consumeValue(decoder, 0) != nil {
		return false
	}
	_, err := decoder.Token()
	return errors.Is(err, io.EOF)
}

func consumeValue(decoder *json.Decoder, depth int) error {
	if depth > 64 {
		return ErrProtocol
	}
	token, err := decoder.Token()
	if err != nil {
		return ErrProtocol
	}
	if number, ok := token.(json.Number); ok {
		value, parseErr := strconv.ParseFloat(string(number), 64)
		if parseErr != nil || math.IsInf(value, 0) || math.IsNaN(value) {
			return ErrProtocol
		}
	}
	delim, compound := token.(json.Delim)
	if !compound {
		return nil
	}
	end := json.Delim(']')
	if delim == '{' {
		end = '}'
	} else if delim != '[' {
		return ErrProtocol
	}
	seen := map[string]struct{}{}
	for decoder.More() {
		if delim == '{' {
			key, keyErr := decoder.Token()
			if keyErr != nil {
				return ErrProtocol
			}
			name, ok := key.(string)
			if !ok {
				return ErrProtocol
			}
			if _, exists := seen[name]; exists {
				return ErrProtocol
			}
			seen[name] = struct{}{}
		}
		if consumeValue(decoder, depth+1) != nil {
			return ErrProtocol
		}
	}
	closing, err := decoder.Token()
	if err != nil || closing != end {
		return ErrProtocol
	}
	return nil
}

// validEscapes rejects unpaired JSON surrogates instead of Go's replacement-rune
// recovery, keeping Python and Go UTF-8 interpretation identical.
func validEscapes(data []byte) bool {
	inString := false
	for index := 0; index < len(data); index++ {
		if data[index] == '"' {
			inString = !inString
			continue
		}
		if !inString || data[index] != '\\' {
			continue
		}
		index++
		if index >= len(data) {
			return false
		}
		if data[index] != 'u' {
			continue
		}
		if index+4 >= len(data) {
			return false
		}
		unit, err := strconv.ParseUint(string(data[index+1:index+5]), 16, 16)
		if err != nil {
			return false
		}
		index += 4
		if unit >= 0xdc00 && unit <= 0xdfff {
			return false
		}
		if unit < 0xd800 || unit > 0xdbff {
			continue
		}
		if index+6 >= len(data) || data[index+1] != '\\' || data[index+2] != 'u' {
			return false
		}
		low, err := strconv.ParseUint(string(data[index+3:index+7]), 16, 16)
		if err != nil || low < 0xdc00 || low > 0xdfff {
			return false
		}
		index += 6
	}
	return !inString
}

// Hash uses the existing authoritative ledger fingerprint, without selecting a price.
func (u Usage) Hash() string {
	return (run.UsageReport{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, CachedInputTokens: u.CachedInputTokens, ReceiptHash: u.ReceiptHash}).Hash()
}

func validDisposition(f Frame) bool {
	return (f.UsageDisposition == "unknown" && f.UsageHash == nil) || (f.UsageDisposition == "reported" && f.UsageHash != nil && hashPattern.MatchString(*f.UsageHash))
}
