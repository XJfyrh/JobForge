package diagnosis

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

type diagnosticRoundTripper func(*http.Request) (*http.Response, error)

func (f diagnosticRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestSendPreservesThinkingModeAndMeteringForFinalOnlyGroups(t *testing.T) {
	for _, group := range []string{"B", "V5", "V8", "V8C", "C1", "C2", "V8T"} {
		for _, mode := range []string{"thinking", "empty_reasoning", "zero_usage", "overlap"} {
			t.Run(group+"/"+mode, func(t *testing.T) {
				value := responseFixture(mode != "empty_reasoning")
				details := value["usage"].(map[string]any)["completion_tokens_details"].(map[string]any)
				if mode == "zero_usage" {
					details["reasoning_tokens"] = 0
				} else if mode == "overlap" {
					details["reasoning_tokens"] = 31
				}
				raw, requestBody := encoded(t, value), []byte(`{"fixture":"no-network"}`)
				calls := 0
				client := &http.Client{Transport: diagnosticRoundTripper(func(request *http.Request) (*http.Response, error) {
					calls++
					body, err := io.ReadAll(request.Body)
					if err != nil || !bytes.Equal(body, requestBody) || request.Method != http.MethodPost ||
						request.URL.String() != "https://api.deepseek.com/chat/completions" ||
						request.Header.Get("Authorization") != "Bearer fixture-key" {
						t.Fatal("send changed the fixed request or provider binding")
					}
					return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(raw)), Header: make(http.Header)}, nil
				})}
				report, observed := send(context.Background(), client, requestBody, "fixture-key", Trial{ID: "fixture-call", Group: group})
				if calls != 1 || !bytes.Equal(observed, raw) || report.ResponseSHA256 != Digest(raw) {
					t.Fatal("send must retain one physical response without retry")
				}
				if mode == "zero_usage" || mode == "overlap" {
					if report.Stop == "" || report.Usage != nil {
						t.Fatal("contradictory thinking usage must stop and preserve full hold")
					}
				} else if report.Stop != "" || report.Outcome != "final" || report.Usage == nil ||
					report.Usage.Validate() != nil || report.ReasoningObserved != (mode == "thinking") {
					t.Fatalf("normal final-only response misclassified: %+v", report)
				}
			})
		}
	}
}

func responseFixture(thinking bool) map[string]any {
	message := map[string]any{"role": "assistant", "content": `{"type":"final","proposal":{}}`}
	reasoning := 0
	if thinking {
		message["reasoning_content"] = "fixture reasoning"
		reasoning = 20
	}
	return map[string]any{"id": "fixture-response", "object": "chat.completion", "created": 1,
		"model": "deepseek-flash", "system_fingerprint": "fixture-fp",
		"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": "stop"}},
		"usage": map[string]any{"prompt_tokens": 100, "completion_tokens": 30, "total_tokens": 130,
			"prompt_cache_hit_tokens": 40, "prompt_cache_miss_tokens": 60,
			"completion_tokens_details": map[string]any{"reasoning_tokens": reasoning}}}
}

func encoded(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestProviderModesAndUsage(t *testing.T) {
	for _, thinking := range []bool{false, true} {
		group := "A"
		if thinking {
			group = "B"
		}
		r := ParseReport(encoded(t, responseFixture(thinking)), 200, group, "call")
		if r.Stop != "" || r.Outcome != "final" || r.Usage == nil || r.Usage.OutputTokens != 30 || r.Usage.Validate() != nil {
			t.Fatalf("unexpected report: %+v", r)
		}
	}
	r := ParseReport(encoded(t, responseFixture(true)), 200, "A", "call")
	if r.Stop != "unexpected_thinking" || r.Usage == nil {
		t.Fatal("wrong mode must stop but retain complete metering")
	}
	r = ParseReport(encoded(t, responseFixture(false)), 200, "B", "call")
	if r.Stop != "" || r.Usage == nil || r.Outcome != "final" || r.ReasoningObserved {
		t.Fatal("empty reasoning is an observation, not a mode or billing failure")
	}
}

func TestProviderFailuresRetainAppropriateExposure(t *testing.T) {
	for _, mutation := range []string{"length", "bad_json", "unknown_usage", "bad_total", "reasoning_zero", "reasoning_overlap", "model", "tool", "overbound"} {
		t.Run(mutation, func(t *testing.T) {
			value := responseFixture(true)
			choice := value["choices"].([]any)[0].(map[string]any)
			usage := value["usage"].(map[string]any)
			switch mutation {
			case "length":
				choice["finish_reason"] = "length"
			case "bad_json":
				choice["message"].(map[string]any)["content"] = "bad"
			case "unknown_usage":
				delete(value, "usage")
			case "bad_total":
				usage["total_tokens"] = 1
			case "reasoning_overlap":
				usage["completion_tokens_details"].(map[string]any)["reasoning_tokens"] = 31
			case "reasoning_zero":
				usage["completion_tokens_details"].(map[string]any)["reasoning_tokens"] = 0
			case "model":
				value["model"] = "other"
			case "tool":
				choice["message"].(map[string]any)["tool_calls"] = []any{map[string]any{"name": "write"}}
			case "overbound":
				usage["completion_tokens"] = 20000
				usage["total_tokens"] = 20100
			}
			r := ParseReport(encoded(t, value), 200, "B", "call")
			if mutation == "length" || mutation == "bad_json" {
				if r.Usage == nil || r.Stop != "" || r.Outcome == "final" {
					t.Fatal("known output failures must not lose usage")
				}
			} else if r.Stop == "" {
				t.Fatal("invalid evidence must stop")
			}
			if (mutation == "unknown_usage" || mutation == "bad_total" || mutation == "reasoning_overlap" || mutation == "reasoning_zero" || mutation == "model") && r.Usage != nil {
				t.Fatal("unknown usage or tariff must keep full hold")
			}
		})
	}
	r := ParseReport([]byte(`{"id":"a","id":"b"}`), 200, "A", "call")
	if r.Usage != nil || r.Stop == "" {
		t.Fatal("duplicate keys accepted")
	}
}
