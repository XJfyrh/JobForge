package run_test

import (
	"errors"
	"testing"

	"github.com/xjfyrh/jobforge/internal/run"
)

var budgetDimensions = []struct {
	name string
	set  func(*run.Usage, int64)
}{
	{"chat", func(u *run.Usage, n int64) { u.Chat = n }},
	{"logical_tools", func(u *run.Usage, n int64) { u.LogicalTools = n }},
	{"query_embedding", func(u *run.Usage, n int64) { u.QueryEmbedding = n }},
	{"profile_metadata_http", func(u *run.Usage, n int64) { u.ProfileMetadataHTTP = n }},
	{"business_tool_http", func(u *run.Usage, n int64) { u.BusinessToolHTTP = n }},
	{"physical_http", func(u *run.Usage, n int64) { u.PhysicalHTTP = n }},
	{"protocol_corrections", func(u *run.Usage, n int64) { u.ProtocolCorrections = n }},
	{"tokens", func(u *run.Usage, n int64) { u.Tokens = n }},
	{"cost_microyuan", func(u *run.Usage, n int64) { u.CostMicroyuan = n }},
}

func TestUsageRejectsUnsafeIntegersAndAdditionOverflow(t *testing.T) {
	for _, dimension := range budgetDimensions {
		t.Run(dimension.name, func(t *testing.T) {
			var maximum, one run.Usage
			dimension.set(&maximum, run.MaxSafeInteger)
			dimension.set(&one, 1)
			if !maximum.Valid() {
				t.Fatal("exact safe integer boundary rejected")
			}
			for _, invalid := range []int64{-1, run.MaxSafeInteger + 1} {
				var usage run.Usage
				dimension.set(&usage, invalid)
				if usage.Valid() {
					t.Fatalf("invalid value %d accepted", invalid)
				}
				if _, err := run.AddUsage(usage, run.Usage{}); !errors.Is(err, run.ErrInvalidArgument) {
					t.Fatalf("invalid current amount accepted: %v", err)
				}
				if _, err := run.AddUsage(run.Usage{}, usage); !errors.Is(err, run.ErrInvalidArgument) {
					t.Fatalf("invalid delta accepted: %v", err)
				}
			}
			result, err := run.AddUsage(maximum, one)
			if !errors.Is(err, run.ErrBudgetExhausted) || result != (run.Usage{}) {
				t.Fatalf("overflow must return no partial amount: %+v %v", result, err)
			}
		})
	}
	// The last dimension fails after earlier dimensions could have incremented.
	result, err := run.AddUsage(run.Usage{Chat: 1, CostMicroyuan: run.MaxSafeInteger}, run.Usage{Chat: 1, CostMicroyuan: 1})
	if !errors.Is(err, run.ErrBudgetExhausted) || result != (run.Usage{}) {
		t.Fatalf("late overflow returned a partial addition: %+v %v", result, err)
	}
}

func TestFamilyLimitsAndEveryBudgetDimension(t *testing.T) {
	want := run.Usage{Chat: 12, LogicalTools: 8, QueryEmbedding: 8, ProfileMetadataHTTP: 16,
		BusinessToolHTTP: 8, PhysicalHTTP: 44, ProtocolCorrections: 1, Tokens: 1000, CostMicroyuan: 2000}
	if got := run.FamilyLimits(1000, 2000); got != want {
		t.Fatalf("shared retry-family contract changed: %+v", got)
	}
	for _, dimension := range budgetDimensions {
		t.Run(dimension.name, func(t *testing.T) {
			var delta run.Usage
			dimension.set(&delta, 1)
			if _, err := run.ReserveAccount(run.Account{}, delta); !errors.Is(err, run.ErrBudgetExhausted) {
				t.Fatalf("zero dimension limit ignored: %v", err)
			}
		})
	}
	if _, err := run.ReserveAccount(run.Account{Frozen: true, Limits: want}, run.Usage{}); !errors.Is(err, run.ErrBudgetExhausted) {
		t.Fatalf("frozen account grants authority: %v", err)
	}
}

func TestKnownUsageReleasesOnlyItsHoldAndNeverCallCounts(t *testing.T) {
	initial := run.Account{ID: "family-fixture", Scope: "family", Limits: run.FamilyLimits(1000, 2000)}
	first, err := run.ReserveAccount(initial, run.Usage{Chat: 1, PhysicalHTTP: 1, Tokens: 100, CostMicroyuan: 200})
	if err != nil {
		t.Fatal(err)
	}
	second, err := run.ReserveAccount(first, run.Usage{Chat: 1, PhysicalHTTP: 1, Tokens: 50, CostMicroyuan: 70})
	if err != nil {
		t.Fatal(err)
	}
	settled, err := run.SettleAccount(second, 100, 200, 25, 30)
	if err != nil {
		t.Fatal(err)
	}
	if settled.Used.Chat != 2 || settled.Used.PhysicalHTTP != 2 || settled.HeldTokens != 50 || settled.HeldCostMicroyuan != 70 ||
		settled.KnownTokens != 25 || settled.KnownCostMicroyuan != 30 || settled.Used.Tokens != 75 || settled.Used.CostMicroyuan != 100 || settled.Frozen {
		t.Fatalf("settlement refunded counts or another unknown hold: %+v", settled)
	}
	settled.Limits.Chat, settled.Limits.PhysicalHTTP = 2, 2
	if _, err = run.ReserveAccount(settled, run.Usage{Chat: 1, PhysicalHTTP: 1}); !errors.Is(err, run.ErrBudgetExhausted) {
		t.Fatalf("lower known usage resurrected physical call quota: %v", err)
	}
	if initial.Used != (run.Usage{}) || first.Used.Tokens != 100 || second.HeldTokens != 150 {
		t.Fatal("account inputs were partially mutated")
	}
}

func TestAnomalousUsageFreezesWithoutRefundingAnyDimension(t *testing.T) {
	account, err := run.ReserveAccount(run.Account{Limits: run.FamilyLimits(1000, 1000)},
		run.Usage{Chat: 1, PhysicalHTTP: 1, Tokens: 100, CostMicroyuan: 100})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name         string
		tokens, cost int64
	}{
		{"token_overrun_cost_under", 101, 1},
		{"cost_overrun_tokens_under", 1, 101},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := run.SettleAccount(account, 100, 100, test.tokens, test.cost)
			if err != nil {
				t.Fatal(err)
			}
			want := account
			want.Frozen = true
			if got != want {
				t.Fatalf("anomalous report must retain every unknown hold and freeze: got %+v want %+v", got, want)
			}
			if _, err = run.ReserveAccount(got, run.Usage{}); !errors.Is(err, run.ErrBudgetExhausted) {
				t.Fatalf("anomaly failed to freeze later sends: %v", err)
			}
		})
	}
}

func TestSettlementRejectsBadAccountingAndSafeIntegerOverflow(t *testing.T) {
	account := run.Account{Limits: run.FamilyLimits(run.MaxSafeInteger, run.MaxSafeInteger),
		Used: run.Usage{Tokens: 100, CostMicroyuan: 100}, HeldTokens: 100, HeldCostMicroyuan: 100}
	for index := range 4 {
		for _, invalid := range []int64{-1, run.MaxSafeInteger + 1} {
			values := [4]int64{100, 100, 20, 20}
			values[index] = invalid
			if _, err := run.SettleAccount(account, values[0], values[1], values[2], values[3]); !errors.Is(err, run.ErrInvalidArgument) {
				t.Fatalf("invalid settlement field %d accepted: %v", index, err)
			}
		}
	}
	if _, err := run.SettleAccount(account, 101, 100, 1, 1); !errors.Is(err, run.ErrInternal) {
		t.Fatalf("settlement removed another/nonexistent hold: %v", err)
	}
	maximum := run.Account{Limits: run.FamilyLimits(run.MaxSafeInteger, run.MaxSafeInteger),
		Used:       run.Usage{Tokens: run.MaxSafeInteger, CostMicroyuan: run.MaxSafeInteger},
		HeldTokens: run.MaxSafeInteger, HeldCostMicroyuan: run.MaxSafeInteger}
	settled, err := run.SettleAccount(maximum, run.MaxSafeInteger, run.MaxSafeInteger, run.MaxSafeInteger, run.MaxSafeInteger)
	if err != nil || settled.Used != maximum.Used || settled.HeldTokens != 0 || settled.KnownTokens != run.MaxSafeInteger {
		t.Fatalf("exact maximum settlement changed integer values: %+v %v", settled, err)
	}
	// Existing known usage plus a legitimate new hold cannot overflow when saved.
	bad := account
	bad.KnownTokens = run.MaxSafeInteger
	result, err := run.SettleAccount(bad, 100, 100, 1, 1)
	if !errors.Is(err, run.ErrInternal) || result != (run.Account{}) {
		t.Fatalf("overflow produced partial account settlement: %+v %v", result, err)
	}
}
