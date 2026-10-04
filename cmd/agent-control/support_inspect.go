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
	if len(config.Profiles) != 1 || len(config.Workers) < 1 || len(config.Workers) > 2 || len(config.Budgets) != 3 || len(config.Bindings) != 2 ||
		!run.IsSupportStrategy(config.Profiles[0].Strategy) || !config.Profiles[0].AuditEnabled() {
		return errors.New("inspect-support requires one prepared registered support batch")
	}
	if len(config.Workers) == 2 && !config.Profiles[0].ConfirmedStepRecovery() {
		return errors.New("only S3 accepts a declared replacement worker")
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
	if len(config.Workers) == 2 {
		request.WorkerID = config.Workers[1].ID
		second, err := store.InspectSupport(inspectCtx, request)
		if err != nil {
			return errors.New("read-only replacement inspection failed")
		}
		result.MigrationsReady = result.MigrationsReady && second.MigrationsReady
		result.MatchesConfig = result.MatchesConfig && second.MatchesConfig
		// Count each principal's startup/session facts once. Shared batch and
		// account histories remain a single observation, never doubled costs.
		result.History.WorkerSessions += second.History.WorkerSessions
		result.History.WorkerStartups += second.History.WorkerStartups
	}
	if json.NewEncoder(os.Stdout).Encode(result) != nil {
		return errors.New("support inspection output failed")
	}
	return nil
}
