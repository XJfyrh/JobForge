package httpapi

import (
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/xjfyrh/jobforge/internal/run"
)

// UIOption binds a business-facing name to an existing deployment's admission
// identities. It grants no execution authority or additional budget.
type UIOption struct {
	TenantID      string `json:"tenant_id"`
	Name          string `json:"name"`
	ProfileID     string `json:"profile_id"`
	BudgetBatchID string `json:"budget_batch_id"`
}

func (h *handler) configureUIOptions(options []UIOption) error {
	if len(options) > 512 {
		return run.ErrInvalidArgument
	}
	seen := make(map[string]bool)
	for _, option := range options {
		key := option.TenantID + ":" + option.Name
		if !run.ValidIdentifier(option.TenantID) || !run.ValidIdentifier(option.ProfileID) || !run.ValidIdentifier(option.BudgetBatchID) ||
			!utf8.ValidString(option.Name) || len(option.Name) > 160 || strings.TrimSpace(option.Name) == "" ||
			strings.ContainsFunc(option.Name, unicode.IsControl) || seen[key] {
			return run.ErrInvalidArgument
		}
		seen[key] = true
		h.uiOptions = append(h.uiOptions, option)
	}
	return nil
}

func (h *handler) listUIOptions(w http.ResponseWriter, r *http.Request) {
	identity := authenticated(r)
	if identity.Role != "operator" {
		writeError(w, run.ErrForbidden)
		return
	}
	if _, err := query(r); err != nil {
		writeError(w, err)
		return
	}
	type choice struct {
		Name          string `json:"name"`
		ProfileID     string `json:"profile_id"`
		BudgetBatchID string `json:"budget_batch_id"`
	}
	items := []choice{}
	for _, option := range h.uiOptions {
		if option.TenantID == identity.TenantID {
			items = append(items, choice{option.Name, option.ProfileID, option.BudgetBatchID})
		}
	}
	writeJSON(w, http.StatusOK, struct {
		Items []choice `json:"items"`
	}{items})
}
