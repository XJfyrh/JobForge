package run

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func ledgerProfileFixture() Profile {
	return Profile{ID: "mechanical-v1", Hash: strings.Repeat("a", 64), MaxInputTokens: 1000, MaxOutputTokens: 100,
		Pricing: Pricing{Hash: strings.Repeat("b", 64), Denominator: 100, InputMissMicroyuan: 7, InputHitMicroyuan: 2, OutputMicroyuan: 11}}
}

func TestUsageCostKeepsExactWideIntegerArithmeticAndCeilsOnce(t *testing.T) {
	for _, tc := range []struct {
		name  string
		price Pricing
		usage UsageReport
		want  int64
		err   error
	}{
		{"free", Pricing{Denominator: 1}, UsageReport{InputTokens: 100}, 0, nil},
		{"combined-rounding", Pricing{Denominator: 10, InputMissMicroyuan: 1, OutputMicroyuan: 1}, UsageReport{InputTokens: 1, OutputTokens: 1}, 1, nil},
		{"cache-split", Pricing{Denominator: 10, InputMissMicroyuan: 7, InputHitMicroyuan: 2, OutputMicroyuan: 11}, UsageReport{InputTokens: 10, CachedInputTokens: 4, OutputTokens: 2}, 8, nil},
		{"wide-intermediate", Pricing{Denominator: MaxSafeInteger, InputMissMicroyuan: MaxSafeInteger}, UsageReport{InputTokens: MaxSafeInteger}, MaxSafeInteger, nil},
		{"unrepresentable-exposure", Pricing{Denominator: 1, InputMissMicroyuan: MaxSafeInteger}, UsageReport{InputTokens: 2}, 0, ErrBudgetExhausted},
		{"zero-denominator", Pricing{}, UsageReport{}, 0, ErrInvalidArgument},
		{"negative-price", Pricing{Denominator: 1, InputMissMicroyuan: -1}, UsageReport{}, 0, ErrInvalidArgument},
		{"cache-not-a-subset", Pricing{Denominator: 1}, UsageReport{InputTokens: 1, CachedInputTokens: 2}, 0, ErrInvalidArgument},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cost, err := UsageCost(tc.price, tc.usage)
			if cost != tc.want || !errors.Is(err, tc.err) {
				t.Fatalf("cost=%d err=%v, want=%d err=%v", cost, err, tc.want, tc.err)
			}
		})
	}
}

func TestReservationUsesRegisteredUpperBoundsAndIrrevocableCategories(t *testing.T) {
	profile := ledgerProfileFixture()
	chat, err := ReservationBudget(profile, SubcallChat)
	if err != nil || chat.InputTokens != 1000 || chat.OutputTokens != 100 || chat.TotalTokens != 1100 || chat.CostMicroyuan != 81 {
		t.Fatalf("conservative chat hold: %+v %v", chat, err)
	}
	profile.Pricing.InputHitMicroyuan = 9
	chat, err = ReservationBudget(profile, SubcallChat)
	if err != nil || chat.CostMicroyuan != 101 {
		t.Fatal("upper bound ignored a more expensive cache-hit price")
	}
	usage := ReservationUsage("protocol_correction", SubcallChat, chat)
	if usage.PhysicalHTTP != 1 || usage.Chat != 1 || usage.ProtocolCorrections != 1 || usage.Tokens != chat.TotalTokens || usage.CostMicroyuan != chat.CostMicroyuan {
		t.Fatal("correction did not consume its own physical/chat/correction counters")
	}
	for _, subcall := range []Subcall{SubcallProfileVersion, SubcallProfileTags, SubcallQueryEmbedding, SubcallSearchPolicy, SubcallGetOrder, SubcallGetDelivery} {
		budget, err := ReservationBudget(profile, subcall)
		if err != nil || budget.CostMicroyuan != 0 || budget.OutputTokens != 0 {
			t.Fatalf("fixed local resource reservation: %+v %v", budget, err)
		}
		count := ReservationUsage("search_policy", subcall, budget)
		if count.PhysicalHTTP != 1 || count.Chat != 0 || count.ProtocolCorrections != 0 {
			t.Fatal("free HTTP escaped physical counting or acquired chat identity")
		}
		if subcall == SubcallQueryEmbedding {
			if budget.TotalTokens != 512 || count.QueryEmbedding != 1 {
				t.Fatal("embedding lost bounded token hold")
			}
		} else if budget.TotalTokens != 0 {
			t.Fatal("nonmodel HTTP invented a model token hold")
		}
	}
	for _, maxOutput := range []int64{0, 1025, MaxSafeInteger} {
		profile.MaxOutputTokens = maxOutput
		if _, err := ReservationBudget(profile, SubcallChat); !errors.Is(err, ErrProfileUnavailable) {
			t.Fatal("invalid profile output bound accepted")
		}
	}
}

func TestUsageHashAndTypedJSONPreserveExactMeteringIdentity(t *testing.T) {
	u := UsageReport{InputTokens: MaxSafeInteger - 1, CachedInputTokens: 10, OutputTokens: 1, ReceiptHash: strings.Repeat("c", 64)}
	u.UsageHash = u.Hash()
	body, err := UsageJSON(u)
	if err != nil || !json.Valid(body) {
		t.Fatalf("typed usage JSON: %v", err)
	}
	var decoded UsageReport
	if json.Unmarshal(body, &decoded) != nil || !reflect.DeepEqual(u, decoded) {
		t.Fatal("usage JSON lost exact integers")
	}
	changed := u
	changed.InputTokens++
	if changed.Hash() == u.UsageHash || changed.Validate() == nil {
		t.Fatal("changed usage reused its old receipt identity")
	}
	for _, change := range []func(*UsageReport){
		func(v *UsageReport) { v.InputTokens = -1 },
		func(v *UsageReport) { v.OutputTokens = MaxSafeInteger + 1 },
		func(v *UsageReport) { v.CachedInputTokens = MaxSafeInteger },
		func(v *UsageReport) { v.ReceiptHash = "incomplete" },
	} {
		bad := u
		change(&bad)
		bad.UsageHash = bad.Hash()
		if bad.Validate() == nil {
			t.Fatal("invalid metering became valid by recomputing its hash")
		}
	}
}

func TestObservationSeparatesRejectedOutputFromCompleteValidUsage(t *testing.T) {
	u := UsageReport{InputTokens: 12, OutputTokens: 9, ReceiptHash: strings.Repeat("d", 64)}
	u.UsageHash = u.Hash()
	req := ObserveCallRequest{PhysicalCallID: uuid.NewString(), TransportOutcome: "response", HTTPStatus: 200,
		BusinessOutcome: "rejected", ErrorCode: "MODEL_PROTOCOL_ERROR", UsageKnown: true, Usage: &u}
	if err := req.Validate(); err != nil {
		t.Fatalf("output rejection incorrectly prevented usage: %v", err)
	}
	original := req.Hash()
	req.BusinessOutcome, req.ErrorCode = "accepted", ""
	if req.Hash() == original {
		t.Fatal("observation hash did not bind independent business validity")
	}
	for _, change := range []func(*ObserveCallRequest){
		func(r *ObserveCallRequest) { r.UsageKnown = false },
		func(r *ObserveCallRequest) { r.HTTPStatus = 0 },
		func(r *ObserveCallRequest) { r.ErrorCode = "private response text" },
		func(r *ObserveCallRequest) { r.TransportOutcome = "unknown" },
		func(r *ObserveCallRequest) { r.BusinessOutcome = "unknown" },
	} {
		bad := req
		change(&bad)
		if bad.Validate() == nil {
			t.Fatal("inconsistent observation was accepted")
		}
	}
}

func TestToolSequenceRequiresFourIndividuallyAuthorizedSearchRequests(t *testing.T) {
	want := []Subcall{SubcallProfileVersion, SubcallProfileTags, SubcallQueryEmbedding, SubcallSearchPolicy}
	if !reflect.DeepEqual(ToolSequence("search_policy"), want) {
		t.Fatal("search policy sequence changed")
	}
	sequence := ToolSequence("search_policy")
	sequence[0] = SubcallChat
	if !reflect.DeepEqual(ToolSequence("search_policy"), want) {
		t.Fatal("caller mutated registered tool sequence")
	}
	if ToolSequence("unregistered") != nil || CallKind(Subcall("unregistered")) != "" {
		t.Fatal("unknown capability acquired a category")
	}
}

func TestStepIdentityBindsCursorAndEveryImmutableResource(t *testing.T) {
	r := Run{ProfileID: "registered-v1", ProfileHash: strings.Repeat("a", 64), SnapshotID: uuid.NewString(),
		SnapshotHash: strings.Repeat("b", 64), CursorVersion: 2}
	a := Authority{NextStepID: uuid.NewString(), NextStepKind: "search_policy", NextInputHash: strings.Repeat("c", 64)}
	step := StepIdentity{ID: a.NextStepID, Kind: a.NextStepKind, Sequence: 3, CursorVersion: 2,
		InputHash: a.NextInputHash, ProfileID: r.ProfileID, ProfileHash: r.ProfileHash,
		SnapshotID: r.SnapshotID, SnapshotHash: r.SnapshotHash}
	if err := CheckStep(r, a, step); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*StepIdentity){
		func(s *StepIdentity) { s.ID = uuid.NewString() },
		func(s *StepIdentity) { s.Kind = "get_order" },
		func(s *StepIdentity) { s.Sequence++ },
		func(s *StepIdentity) { s.CursorVersion++ },
		func(s *StepIdentity) { s.InputHash = strings.Repeat("d", 64) },
		func(s *StepIdentity) { s.ProfileID = "different-profile" },
		func(s *StepIdentity) { s.ProfileHash = strings.Repeat("d", 64) },
		func(s *StepIdentity) { s.SnapshotID = uuid.NewString() },
		func(s *StepIdentity) { s.SnapshotHash = strings.Repeat("d", 64) },
	} {
		other := step
		mutate(&other)
		if !errors.Is(CheckStep(r, a, other), ErrStepConflict) {
			t.Fatal("changed step binding was accepted")
		}
	}
	step.Kind, a.NextStepKind = "unregistered", "unregistered"
	if !errors.Is(CheckStep(r, a, step), ErrInvalidArgument) {
		t.Fatal("matching strings admitted an unregistered capability")
	}
}
