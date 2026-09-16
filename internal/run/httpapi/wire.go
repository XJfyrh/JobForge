package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/xjfyrh/jobforge/internal/jsonstrict"
	"github.com/xjfyrh/jobforge/internal/run"
)

const (
	maxRequestBytes  = 4 * 1024
	maxResponseBytes = 2 * 1024 * 1024
)

func decodeJSON(w http.ResponseWriter, r *http.Request, target any) error {
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" || len(r.Header.Values("Content-Type")) != 1 ||
		(len(params) > 0 && (len(params) != 1 || !strings.EqualFold(params["charset"], "utf-8"))) {
		return run.ErrInvalidArgument
	}
	if encoding := r.Header.Get("Content-Encoding"); encoding != "" && encoding != "identity" {
		return run.ErrInvalidArgument
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return run.ErrInvalidArgument
	}
	if err := jsonstrict.Decode(body, target); err != nil {
		return run.ErrInvalidArgument
	}
	return nil
}

func operationKey(r *http.Request) (string, error) {
	values := r.Header.Values("Idempotency-Key")
	if len(values) != 1 || !run.ValidIdentifier(values[0]) {
		return "", run.ErrInvalidArgument
	}
	return values[0], nil
}

func runID(r *http.Request) (string, error) {
	value := chi.URLParam(r, "run_id")
	parsed, err := uuid.Parse(value)
	if err != nil || parsed.String() != value {
		return "", run.ErrInvalidArgument
	}
	return value, nil
}

func query(r *http.Request, allowed ...string) (url.Values, error) {
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(r.URL.RawQuery) > 8192 {
		return nil, run.ErrInvalidArgument
	}
	for name, entries := range values {
		permitted := false
		for _, candidate := range allowed {
			permitted = permitted || name == candidate
		}
		if !permitted || len(entries) != 1 || entries[0] == "" {
			return nil, run.ErrInvalidArgument
		}
	}
	return values, nil
}

func integerQuery(values url.Values, name string, fallback, low, high int64) (int64, error) {
	value, exists := values[name]
	if !exists {
		return fallback, nil
	}
	// Decimal query integers are canonical: no sign, leading zeros or exponent.
	parsed, err := strconv.ParseInt(value[0], 10, 64)
	if err != nil || parsed < low || parsed > high || strconv.FormatInt(parsed, 10) != value[0] {
		return 0, run.ErrInvalidArgument
	}
	return parsed, nil
}

func historyQuery(r *http.Request) (after int64, limit int, err error) {
	values, err := query(r, "after", "limit")
	if err != nil {
		return 0, 0, err
	}
	after, err = integerQuery(values, "after", 0, 0, run.MaxSafeInteger)
	if err != nil {
		return 0, 0, err
	}
	count, err := integerQuery(values, "limit", 20, 1, 100)
	return after, int(count), err
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	body, err := json.Marshal(value)
	if err != nil || len(body) > maxResponseBytes {
		writeError(w, run.ErrInternal)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(body, '\n'))
}

func writeError(w http.ResponseWriter, err error) {
	code, status := publicError(err)
	// Never expose err.Error(): wrapped errors may contain SQL or credentials.
	envelope := struct {
		Error run.Failure `json:"error"`
	}{Error: run.Failure{Code: string(code), Message: strings.ToLower(strings.ReplaceAll(string(code), "_", " "))}}
	writeJSON(w, status, envelope)
}

func publicError(err error) (run.ErrorCode, int) {
	var code run.ErrorCode
	if !errors.As(err, &code) {
		return run.ErrInternal, http.StatusInternalServerError
	}
	switch code {
	case run.ErrInvalidArgument:
		return code, http.StatusBadRequest
	case run.ErrUnauthorized:
		return code, http.StatusUnauthorized
	case run.ErrForbidden:
		return code, http.StatusForbidden
	case run.ErrNotFound:
		return code, http.StatusNotFound
	case run.ErrConflict, run.ErrAlreadyTerminal, run.ErrInvalidTransition,
		run.ErrProfileUnavailable, run.ErrBudgetExhausted:
		return code, http.StatusConflict
	case run.ErrQueueOverloaded, run.ErrorCode("RATE_LIMITED"):
		return code, http.StatusTooManyRequests
	case run.ErrDependencyUnavailable:
		return code, http.StatusServiceUnavailable
	default:
		return run.ErrInternal, http.StatusInternalServerError
	}
}
