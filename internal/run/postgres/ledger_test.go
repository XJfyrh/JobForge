package postgres

import (
	"errors"
	"reflect"
	"testing"
	"time"

	agentrun "github.com/xjfyrh/jobforge/internal/run"
)

func TestThreeLedgerAccountsReserveAllOrNone(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	var accounts [3]budgetRow
	for i, scope := range []string{"family", "tenant", "batch"} {
		accounts[i] = budgetRow{Account: agentrun.Account{ID: scope, Scope: scope,
			Limits: agentrun.FamilyLimits(100, 100)}, ValidFrom: now.Add(-time.Hour), ValidUntil: now.Add(time.Hour)}
	}
	delta := agentrun.Usage{Chat: 1, PhysicalHTTP: 1, Tokens: 100, CostMicroyuan: 100}
	reserved, err := reserveLedgerAccounts(accounts, delta, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range reserved {
		if row.Account.Used != delta || row.Account.HeldTokens != 100 || row.Account.HeldCostMicroyuan != 100 {
			t.Fatal("one budget layer did not receive the complete reservation")
		}
	}
	if accounts[0].Account.Used != (agentrun.Usage{}) {
		t.Fatal("reservation mutated caller-owned accounts")
	}
	for _, name := range []string{"batch-limit", "batch-expiry-equality", "tenant-frozen", "family-not-active"} {
		t.Run(name, func(t *testing.T) {
			input := accounts
			switch name {
			case "batch-limit":
				input[2].Account.Limits.Chat = 0
			case "batch-expiry-equality":
				input[2].ValidUntil = now
			case "tenant-frozen":
				input[1].Account.Frozen = true
			case "family-not-active":
				input[0].ValidFrom = now.Add(time.Nanosecond)
			}
			returned, err := reserveLedgerAccounts(input, delta, now)
			if !errors.Is(err, agentrun.ErrBudgetExhausted) || !reflect.DeepEqual(returned, input) {
				t.Fatal("failed reservation left a partial family or tenant hold")
			}
		})
	}
}
