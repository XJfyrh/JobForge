// Package ctl implements the jobforge ctl operational CLI (PRD v0.2
// FR-620/621). It is a pure client of the existing HTTP control plane:
// no server-side privileged paths are introduced. Authentication uses the
// same Bearer API key scheme as the HTTP API; the tenant identity is always
// derived server-side from the key (PRD v0.1 §11.4).
//
// Logging discipline (PRD v0.2 NFR-205): this package never logs API keys,
// Authorization headers or full job payloads.
package ctl

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Job mirrors the HTTP API job response (GET /v1/jobs/{job_id}).
type Job struct {
	ID             string          `json:"id"`
	TenantID       string          `json:"tenant_id"`
	Queue          string          `json:"queue"`
	Type           string          `json:"type"`
	Payload        json.RawMessage `json:"payload"`
	Priority       int16           `json:"priority"`
	State          string          `json:"state"`
	RunAt          time.Time       `json:"run_at"`
	Attempt        int             `json:"attempt"`
	MaxAttempts    int             `json:"max_attempts"`
	TimeoutSeconds int             `json:"timeout_seconds"`
	IdempotencyKey *string         `json:"idempotency_key"`
	LeaseOwner     *string         `json:"lease_owner"`
	LeaseUntil     *time.Time      `json:"lease_until"`
	FencingToken   int64           `json:"fencing_token"`
	TraceID        *string         `json:"trace_id"`
	RetryOfJobID   *string         `json:"retry_of_job_id"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
	Attempts       []Attempt       `json:"attempts,omitempty"`
}

// Attempt mirrors one entry of the attempt timeline.
type Attempt struct {
	AttemptNo    int        `json:"attempt_no"`
	WorkerID     string     `json:"worker_id"`
	FencingToken int64      `json:"fencing_token"`
	StartedAt    time.Time  `json:"started_at"`
	FinishedAt   *time.Time `json:"finished_at,omitempty"`
	Outcome      string     `json:"outcome"`
	ErrorCode    *string    `json:"error_code,omitempty"`
	ErrorMessage *string    `json:"error_message,omitempty"`
	DurationMs   *int64     `json:"duration_ms,omitempty"`
}

// ListResult mirrors the GET /v1/jobs response.
type ListResult struct {
	Jobs       []Job  `json:"jobs"`
	NextCursor string `json:"next_cursor,omitempty"`
}

// SubmitResult mirrors the POST /v1/jobs and :retry responses.
type SubmitResult struct {
	JobID        string `json:"job_id"`
	State        string `json:"state"`
	Deduplicated bool   `json:"deduplicated"`
}

// ListOptions holds list filtering and pagination options.
type ListOptions struct {
	State  string
	Queue  string
	Type   string
	Limit  int
	Cursor string
}

// APIError is a server-side error surfaced by the control plane. Code is
// the JobForge error code (e.g. NOT_FOUND, ALREADY_TERMINAL).
type APIError struct {
	StatusCode int
	Code       string
	Message    string
}

// Error implements the error interface.
func (e *APIError) Error() string {
	return fmt.Sprintf("%s: %s (HTTP %d)", e.Code, e.Message, e.StatusCode)
}

// errorEnvelope mirrors the server error response shape.
type errorEnvelope struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// Client is an HTTP client for the JobForge control plane API.
type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

// NewClient creates a control plane client. baseURL must include the scheme
// (e.g. http://localhost:8080). The API key is sent as a Bearer token and
// must never be logged.
//
// The destination is always operator-supplied (--api-url flag or
// JOBFORGE_API_URL env), never derived from untrusted input; loopback and
// internal addresses are the intended targets for this operational tool.
func NewClient(baseURL, apiKey string) *Client {
	return &Client{
		baseURL: baseURL,
		apiKey:  apiKey,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

// List retrieves jobs matching the options with keyset pagination.
func (c *Client) List(ctx context.Context, opts ListOptions) (*ListResult, error) {
	q := url.Values{}
	if opts.State != "" {
		q.Set("state", opts.State)
	}
	if opts.Queue != "" {
		q.Set("queue", opts.Queue)
	}
	if opts.Type != "" {
		q.Set("type", opts.Type)
	}
	if opts.Limit > 0 {
		q.Set("limit", strconv.Itoa(opts.Limit))
	}
	if opts.Cursor != "" {
		q.Set("cursor", opts.Cursor)
	}

	path := "/v1/jobs"
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}

	var res ListResult
	if err := c.do(ctx, http.MethodGet, path, nil, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// Get retrieves job details together with the attempt timeline.
func (c *Client) Get(ctx context.Context, jobID string) (*Job, error) {
	var job Job
	if err := c.do(ctx, http.MethodGet, "/v1/jobs/"+url.PathEscape(jobID), nil, &job); err != nil {
		return nil, err
	}
	return &job, nil
}

// Cancel requests cancellation of a job.
func (c *Client) Cancel(ctx context.Context, jobID string) error {
	var res struct {
		Status string `json:"status"`
	}
	return c.do(ctx, http.MethodPost, "/v1/jobs/"+url.PathEscape(jobID)+":cancel", nil, &res)
}

// Retry manually retries a dead/cancelled job. The server clones it as a new
// job (new job_id, retry_of_job_id audit link); the original terminal state
// is never modified (ADR-0001).
func (c *Client) Retry(ctx context.Context, jobID string) (*SubmitResult, error) {
	var res SubmitResult
	if err := c.do(ctx, http.MethodPost, "/v1/jobs/"+url.PathEscape(jobID)+":retry", nil, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// do performs one authenticated request and decodes the JSON response into
// out. Non-2xx responses are mapped to *APIError.
func (c *Client) do(ctx context.Context, method, path string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("request %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Read a bounded body; control plane responses are small.
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		apiErr := &APIError{StatusCode: resp.StatusCode, Code: "INTERNAL", Message: string(data)}
		var env errorEnvelope
		if jsonErr := json.Unmarshal(data, &env); jsonErr == nil && env.Error.Code != "" {
			apiErr.Code = env.Error.Code
			apiErr.Message = env.Error.Message
		}
		return apiErr
	}

	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}
