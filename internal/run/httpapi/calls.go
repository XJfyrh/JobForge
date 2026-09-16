package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/xjfyrh/jobforge/internal/run"
)

// calls projects only tenant-authorized facts. No query knobs, credentials,
// execution authority or audit mutations belong to this read route.
func (h *handler) calls(w http.ResponseWriter, r *http.Request) {
	id, err := detailRequest(r)
	if err != nil {
		writeError(w, err)
		return
	}
	value, err := h.api.Calls(r.Context(), authenticated(r).TenantID, id)
	if err != nil {
		writeError(w, err)
		return
	}
	if len(value.Items) > run.MaxCallsPerRun {
		writeError(w, run.ErrInternal)
		return
	}
	if value.Items == nil {
		value.Items = []run.CallView{}
	}
	body, err := json.Marshal(value)
	if err != nil || len(body)+1 > run.MaxCallsResponseBytes {
		writeError(w, run.ErrInternal)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(append(body, '\n'))
}
