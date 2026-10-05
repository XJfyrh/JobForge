package httpapi

import (
	"context"
	"net/http"

	"github.com/xjfyrh/jobforge/internal/run"
)

type actionAPI interface {
	run.ActionStore
	Reconcile(context.Context, string, string) (run.EffectView, error)
}

func (h *handler) approval(w http.ResponseWriter, r *http.Request) {
	id, err := detailRequest(r)
	if err != nil {
		writeError(w, err)
		return
	}
	api, ok := h.api.(actionAPI)
	if !ok {
		writeError(w, run.ErrProfileUnavailable)
		return
	}
	view, err := api.Approval(r.Context(), authenticated(r).TenantID, id)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}
func (h *handler) decideApproval(w http.ResponseWriter, r *http.Request) {
	i := authenticated(r)
	id, err := detailRequest(r)
	if err != nil {
		writeError(w, err)
		return
	}
	if _, err := h.api.Get(r.Context(), i.TenantID, id); err != nil {
		writeError(w, err)
		return
	}
	if i.Role != "approver" {
		writeError(w, run.ErrForbidden)
		return
	}
	key, err := operationKey(r)
	if err != nil {
		writeError(w, err)
		return
	}
	var request run.ApprovalRequest
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, err)
		return
	}
	if err := request.Validate(); err != nil {
		writeError(w, err)
		return
	}
	api, ok := h.api.(actionAPI)
	if !ok {
		writeError(w, run.ErrProfileUnavailable)
		return
	}
	result, err := api.DecideApproval(r.Context(), i.TenantID, id, i.ActorID, key, request)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
func (h *handler) effect(w http.ResponseWriter, r *http.Request) {
	id, err := detailRequest(r)
	if err != nil {
		writeError(w, err)
		return
	}
	api, ok := h.api.(actionAPI)
	if !ok {
		writeError(w, run.ErrProfileUnavailable)
		return
	}
	result, err := api.Effect(r.Context(), authenticated(r).TenantID, id)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
func (h *handler) reconcile(w http.ResponseWriter, r *http.Request) {
	i := authenticated(r)
	id, err := detailRequest(r)
	if err != nil {
		writeError(w, err)
		return
	}
	if _, err := h.api.Get(r.Context(), i.TenantID, id); err != nil {
		writeError(w, err)
		return
	}
	if i.Role != "operator" {
		writeError(w, run.ErrForbidden)
		return
	}
	var request struct {
		SchemaVersion int `json:"schema_version"`
	}
	if err := decodeJSON(w, r, &request); err != nil || request.SchemaVersion != 1 {
		writeError(w, run.ErrInvalidArgument)
		return
	}
	api, ok := h.api.(actionAPI)
	if !ok {
		writeError(w, run.ErrProfileUnavailable)
		return
	}
	result, err := api.Reconcile(r.Context(), i.TenantID, id)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
func (h *handler) actionCalls(w http.ResponseWriter, r *http.Request) {
	id, err := detailRequest(r)
	if err != nil {
		writeError(w, err)
		return
	}
	api, ok := h.api.(actionAPI)
	if !ok {
		writeError(w, run.ErrProfileUnavailable)
		return
	}
	result, err := api.ActionCalls(r.Context(), authenticated(r).TenantID, id)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
