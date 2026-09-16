package httpapi

import (
	"net/http"

	"github.com/xjfyrh/jobforge/internal/run"
)

func (h *handler) submit(w http.ResponseWriter, r *http.Request) {
	identity := authenticated(r)
	if identity.Role != "operator" {
		writeError(w, run.ErrForbidden)
		return
	}
	if _, err := query(r); err != nil {
		writeError(w, err)
		return
	}
	key, err := operationKey(r)
	if err != nil {
		writeError(w, err)
		return
	}
	request := run.SubmitRequest{RunTimeoutSeconds: 3600}
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, err)
		return
	}
	if err := request.Validate(); err != nil {
		writeError(w, err)
		return
	}
	response, err := h.api.Submit(r.Context(), identity.TenantID, key, request)
	if err != nil {
		writeError(w, err)
		return
	}
	writeSubmission(w, response)
}

func (h *handler) get(w http.ResponseWriter, r *http.Request) {
	id, err := detailRequest(r)
	if err != nil {
		writeError(w, err)
		return
	}
	value, err := h.api.Get(r.Context(), authenticated(r).TenantID, id)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, value)
}

func (h *handler) list(w http.ResponseWriter, r *http.Request) {
	values, err := query(r, "state", "cursor", "limit")
	if err != nil {
		writeError(w, err)
		return
	}
	limit, err := integerQuery(values, "limit", 20, 1, 100)
	if err != nil {
		writeError(w, err)
		return
	}
	filter := run.ListFilter{State: run.State(values.Get("state")), Cursor: values.Get("cursor"), Limit: int(limit)}
	if (filter.State != "" && !filter.State.Valid()) || len(filter.Cursor) > 4096 {
		writeError(w, run.ErrInvalidArgument)
		return
	}
	value, err := h.api.List(r.Context(), authenticated(r).TenantID, filter)
	if err != nil {
		writeError(w, err)
		return
	}
	if value.Items == nil {
		value.Items = []run.Run{}
	}
	writeJSON(w, http.StatusOK, value)
}

func (h *handler) steps(w http.ResponseWriter, r *http.Request) {
	id, err := runID(r)
	if err != nil {
		writeError(w, err)
		return
	}
	after, limit, err := historyQuery(r)
	if err != nil {
		writeError(w, err)
		return
	}
	value, err := h.api.Steps(r.Context(), authenticated(r).TenantID, id, after, limit)
	if err != nil {
		writeError(w, err)
		return
	}
	if value.Items == nil {
		value.Items = []run.Step{}
	}
	writeJSON(w, http.StatusOK, value)
}

func (h *handler) events(w http.ResponseWriter, r *http.Request) {
	id, err := runID(r)
	if err != nil {
		writeError(w, err)
		return
	}
	after, limit, err := historyQuery(r)
	if err != nil {
		writeError(w, err)
		return
	}
	value, err := h.api.Events(r.Context(), authenticated(r).TenantID, id, after, limit)
	if err != nil {
		writeError(w, err)
		return
	}
	if value.Items == nil {
		value.Items = []run.Event{}
	}
	writeJSON(w, http.StatusOK, value)
}

func (h *handler) result(w http.ResponseWriter, r *http.Request) {
	id, err := detailRequest(r)
	if err != nil {
		writeError(w, err)
		return
	}
	value, err := h.api.Result(r.Context(), authenticated(r).TenantID, id)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, value)
}

func (h *handler) cancel(w http.ResponseWriter, r *http.Request) {
	id, key, err := h.operationRequest(r)
	if err != nil {
		writeError(w, err)
		return
	}
	var request struct {
		SchemaVersion int `json:"schema_version"`
	}
	if err := decodeJSON(w, r, &request); err != nil || request.SchemaVersion != 1 {
		writeError(w, run.ErrInvalidArgument)
		return
	}
	value, err := h.api.Cancel(r.Context(), authenticated(r).TenantID, id, key)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, value)
}

func (h *handler) retry(w http.ResponseWriter, r *http.Request) {
	id, key, err := h.operationRequest(r)
	if err != nil {
		writeError(w, err)
		return
	}
	request := run.RetryRequest{RunTimeoutSeconds: 3600}
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, err)
		return
	}
	if err := request.Validate(); err != nil {
		writeError(w, err)
		return
	}
	value, err := h.api.Retry(r.Context(), authenticated(r).TenantID, id, key, request)
	if err != nil {
		writeError(w, err)
		return
	}
	writeSubmission(w, value)
}

func detailRequest(r *http.Request) (string, error) {
	if _, err := query(r); err != nil {
		return "", err
	}
	return runID(r)
}

func (h *handler) operationRequest(r *http.Request) (id, key string, err error) {
	id, err = detailRequest(r)
	if err != nil {
		return "", "", err
	}
	identity := authenticated(r)
	if identity.Role != "operator" {
		// A foreign resource stays undiscoverable even to an authenticated reader.
		// The service performs the same tenant check for operator mutations.
		if _, err := h.api.Get(r.Context(), identity.TenantID, id); err != nil {
			return "", "", err
		}
		return "", "", run.ErrForbidden
	}
	key, err = operationKey(r)
	return id, key, err
}

func writeSubmission(w http.ResponseWriter, response run.SubmitResponse) {
	status := http.StatusCreated
	if response.Reused {
		status = http.StatusOK
	}
	writeJSON(w, status, response)
}
