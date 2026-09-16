package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"time"

	"github.com/xjfyrh/jobforge/internal/run"
	runpostgres "github.com/xjfyrh/jobforge/internal/run/postgres"
)

func inspectSupport(ctx context.Context, store *runpostgres.Store, config deployment) error {
	if len(config.Profiles) != 1 || len(config.Workers) != 1 || len(config.Budgets) != 3 || len(config.Bindings) != 2 ||
		!run.IsSupportStrategy(config.Profiles[0].Strategy) || !config.Profiles[0].AuditEnabled() {
		return errors.New("inspect-support requires one prepared registered support batch")
	}
	request := runpostgres.SupportInspectionRequest{ProfileID: config.Profiles[0].ID, WorkerID: config.Workers[0].ID}
	for _, b := range config.Budgets {
		request.Budgets = append(request.Budgets, run.BudgetSpec{ID: b.ID, Scope: b.Scope, Key: b.Key, ValidFrom: b.ValidFrom, ValidUntil: b.ValidUntil, Limits: b.Limits})
	}
	for _, b := range config.Bindings {
		request.Bindings = append(request.Bindings, runpostgres.SupportBudgetBinding{TenantID: b.TenantID, BatchAccountID: b.BatchAccountID, TenantAccountID: b.TenantAccountID})
	}
	inspectCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	result, err := store.InspectSupport(inspectCtx, request)
	if err != nil {
		return errors.New("read-only support inspection failed")
	}
	if json.NewEncoder(os.Stdout).Encode(result) != nil {
		return errors.New("support inspection output failed")
	}
	return nil
}
