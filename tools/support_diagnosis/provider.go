package diagnosis

import (
	"encoding/json"
	"strings"

	"github.com/xjfyrh/jobforge/internal/jsonstrict"
	"github.com/xjfyrh/jobforge/internal/run"
)

// Report separates complete priceable usage from output correctness and stop rules.
type Report struct {
	ResponseSHA256    string           `json:"response_sha256"`
	ResponseID        string           `json:"response_id"`
	Fingerprint       string           `json:"system_fingerprint"`
	Usage             *run.UsageReport `json:"usage"`
	ReasoningTokens   *int64           `json:"reasoning_tokens"`
	ReasoningObserved bool             `json:"reasoning_observed"`
	Outcome           string           `json:"outcome"`
	Stop              string           `json:"stop"`
}

type providerUsage struct {
	Prompt        *int64 `json:"prompt_tokens"`
	Completion    *int64 `json:"completion_tokens"`
	Total         *int64 `json:"total_tokens"`
	Hit           *int64 `json:"prompt_cache_hit_tokens"`
	Miss          *int64 `json:"prompt_cache_miss_tokens"`
	PromptDetails *struct {
		Cached *int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionDetails *struct {
		Reasoning *int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

type envelope struct {
	ID          string         `json:"id"`
	Object      string         `json:"object"`
	Created     *int64         `json:"created"`
	Model       string         `json:"model"`
	Fingerprint string         `json:"system_fingerprint"`
	Usage       *providerUsage `json:"usage"`
	Choices     []struct {
		Index   int `json:"index"`
		Message struct {
			Role      string          `json:"role"`
			Content   *string         `json:"content"`
			Reasoning *string         `json:"reasoning_content"`
			ToolCalls json.RawMessage `json:"tool_calls"`
		} `json:"message"`
		FinishReason string          `json:"finish_reason"`
		Logprobs     json.RawMessage `json:"logprobs"`
	} `json:"choices"`
}

func safe(n *int64) bool { return n != nil && *n >= 0 && *n <= run.MaxSafeInteger }

// ParseReport never counts reasoning twice and never converts missing usage to zero.
func ParseReport(raw []byte, status int, group, callID string) Report {
	r := Report{ResponseSHA256: Digest(raw), Outcome: "provider_invalid", Stop: "provider_invalid"}
	var e envelope
	if status != 200 || len(raw) > maxResponseBytes || jsonstrict.Decode(raw, &e) != nil ||
		e.Object != "chat.completion" || e.Model != "deepseek-flash" || !run.ValidIdentifier(e.ID) ||
		!run.ValidIdentifier(e.Fingerprint) || !safe(e.Created) {
		return r
	}
	r.ResponseID, r.Fingerprint = e.ID, e.Fingerprint
	u := e.Usage
	if u == nil || !safe(u.Prompt) || !safe(u.Completion) || !safe(u.Total) || !safe(u.Hit) || !safe(u.Miss) ||
		*u.Prompt > run.MaxSafeInteger-*u.Completion || *u.Total != *u.Prompt+*u.Completion ||
		*u.Hit > *u.Prompt || *u.Miss != *u.Prompt-*u.Hit ||
		u.PromptDetails != nil && (!safe(u.PromptDetails.Cached) || *u.PromptDetails.Cached != *u.Hit) {
		r.Outcome, r.Stop = "usage_unknown", "usage_unknown"
		return r
	}
	if u.CompletionDetails != nil {
		if !safe(u.CompletionDetails.Reasoning) || *u.CompletionDetails.Reasoning > *u.Completion {
			return r
		}
		r.ReasoningTokens = u.CompletionDetails.Reasoning
	}
	r.Usage = &run.UsageReport{InputTokens: *u.Prompt, OutputTokens: *u.Completion, CachedInputTokens: *u.Hit,
		ReceiptHash: run.Fingerprint("jobforge.diagnosis.receipt.v1", callID, r.ResponseSHA256, e.ID, e.Fingerprint)}
	r.Usage.UsageHash = r.Usage.Hash()
	if *u.Prompt > maxInputTokens || *u.Completion > maxOutputTokens {
		r.Outcome, r.Stop = "measurement_anomaly", "measurement_anomaly"
		return r
	}
	if len(e.Choices) != 1 || e.Choices[0].Index != 0 || e.Choices[0].Message.Role != "assistant" {
		return r
	}
	c := e.Choices[0]
	if len(c.Logprobs) != 0 && string(c.Logprobs) != "null" {
		return r
	}
	if len(c.Message.ToolCalls) != 0 && string(c.Message.ToolCalls) != "null" && string(c.Message.ToolCalls) != "[]" {
		return r
	}
	if group != "B" && ((r.ReasoningTokens != nil && *r.ReasoningTokens != 0) ||
		c.Message.Reasoning != nil && *c.Message.Reasoning != "") {
		r.Outcome, r.Stop = "unexpected_thinking", "unexpected_thinking"
		return r
	}
	r.ReasoningObserved = c.Message.Reasoning != nil && strings.TrimSpace(*c.Message.Reasoning) != ""
	if group == "B" && r.ReasoningObserved && r.ReasoningTokens != nil && *r.ReasoningTokens == 0 {
		r.Usage = nil
		r.Outcome, r.Stop = "usage_unknown", "usage_unknown"
		return r
	}
	r.Stop = ""
	r.Outcome = "invalid_final"
	if c.FinishReason != "stop" {
		r.Outcome = "incomplete_output"
		return r
	}
	if c.Message.Content == nil || len(*c.Message.Content) > 16384 {
		return r
	}
	var value map[string]json.RawMessage
	if jsonstrict.Decode([]byte(*c.Message.Content), &value) != nil {
		return r
	}
	var kind string
	if len(value) != 2 || json.Unmarshal(value["type"], &kind) != nil || kind != "final" || value["proposal"] == nil {
		return r
	}
	r.Outcome = "final"
	return r
}
