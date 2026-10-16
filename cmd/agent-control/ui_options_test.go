package main

import (
	"strings"
	"testing"

	"github.com/xjfyrh/jobforge/internal/run"
	"github.com/xjfyrh/jobforge/internal/run/httpapi"
)

func TestUIOptionsDoNotEnableDisabledProfiles(t *testing.T) {
	config := deployment{
		Profiles: []run.Profile{{ID: "disabled"}, {ID: "enabled", Executable: true}},
		UIOptions: []httpapi.UIOption{
			{TenantID: "north", Name: "停用方式", ProfileID: "disabled", BudgetBatchID: "batch"},
			{TenantID: "north", Name: "启用方式", ProfileID: "enabled", BudgetBatchID: "batch"},
		},
	}
	options := executableUIOptions(config)
	if len(options) != 1 || options[0].Name != "启用方式" || config.Profiles[0].Executable {
		t.Fatal("UI options changed profile admission or exposed a disabled choice")
	}
}

func TestUIDeploymentBindingMustExist(t *testing.T) {
	body := strings.Replace(emptyDeployment, `"bindings":[]`, `"bindings":[],"ui_options":[{"tenant_id":"tenant-a","name":"处理","profile_id":"missing","budget_batch_id":"missing"}]`, 1)
	if _, err := readDeployment(deploymentFile(t, body)); err == nil {
		t.Fatal("UI option accepted without a registered profile and budget binding")
	}
}
