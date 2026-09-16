package run

// MaxSafeInteger is the common exact integer boundary of JSON clients and SQL.
const MaxSafeInteger int64 = 1<<53 - 1

// FamilyLimits fixes the shared retry-family counters. Money and token limits
// are administrator-selected; they are never derived from an untrusted payload.
func FamilyLimits(tokens, microyuan int64) Usage {
	return Usage{Chat: 12, LogicalTools: 8, QueryEmbedding: 8, ProfileMetadataHTTP: 16,
		BusinessToolHTTP: 8, PhysicalHTTP: 44, ProtocolCorrections: 1,
		Tokens: tokens, CostMicroyuan: microyuan}
}

func usageFields(u *Usage) []*int64 {
	return []*int64{&u.Chat, &u.LogicalTools, &u.QueryEmbedding, &u.ProfileMetadataHTTP,
		&u.BusinessToolHTTP, &u.PhysicalHTTP, &u.ProtocolCorrections, &u.Tokens, &u.CostMicroyuan}
}

// Valid rejects negative or lossy wire integers before any account is mutated.
func (u Usage) Valid() bool {
	for _, n := range usageFields(&u) {
		if *n < 0 || *n > MaxSafeInteger {
			return false
		}
	}
	return true
}

// AddUsage checks all fields before returning a new value; failures never leave
// a partially incremented account that could be saved by a caller.
func AddUsage(current, delta Usage) (Usage, error) {
	if !current.Valid() || !delta.Valid() {
		return Usage{}, ErrInvalidArgument
	}
	out := current
	values, additions := usageFields(&out), usageFields(&delta)
	for i, value := range values {
		if *additions[i] > MaxSafeInteger-*value {
			return Usage{}, ErrBudgetExhausted
		}
		*value += *additions[i]
	}
	return out, nil
}

// ReserveAccount applies a prospective hold. The service must persist all
// three account updates and the call record in the same database transaction.
func ReserveAccount(current Account, delta Usage) (Account, error) {
	if current.Frozen {
		return Account{}, ErrBudgetExhausted
	}
	if !current.Limits.Valid() {
		return Account{}, ErrInternal
	}
	used, err := AddUsage(current.Used, delta)
	if err != nil {
		return Account{}, err
	}
	values, limits := usageFields(&used), usageFields(&current.Limits)
	for i, n := range values {
		if *n > *limits[i] {
			return Account{}, ErrBudgetExhausted
		}
	}
	if current.HeldTokens < 0 || current.HeldCostMicroyuan < 0 ||
		current.HeldTokens > MaxSafeInteger-delta.Tokens || current.HeldCostMicroyuan > MaxSafeInteger-delta.CostMicroyuan {
		return Account{}, ErrInternal
	}
	current.Used = used
	current.HeldTokens += delta.Tokens
	current.HeldCostMicroyuan += delta.CostMicroyuan
	return current, nil
}

// SettleAccount replaces one hold with trusted known usage without refunding
// call counts. The service first checks the call's immutable settlement hash.
// An over-bound observation is retained and freezes subsequent reservations.
func SettleAccount(current Account, heldTokens, heldCost, knownTokens, knownCost int64) (Account, error) {
	values := []int64{heldTokens, heldCost, knownTokens, knownCost, current.KnownTokens,
		current.KnownCostMicroyuan, current.HeldTokens, current.HeldCostMicroyuan}
	for _, n := range values {
		if n < 0 || n > MaxSafeInteger {
			return Account{}, ErrInvalidArgument
		}
	}
	if heldTokens > current.HeldTokens || heldCost > current.HeldCostMicroyuan ||
		knownTokens > MaxSafeInteger-current.KnownTokens || knownCost > MaxSafeInteger-current.KnownCostMicroyuan {
		return Account{}, ErrInternal
	}
	if knownTokens > heldTokens || knownCost > heldCost {
		// An anomalous report does not prove a safe refund in either dimension.
		// The ledger retains the raw report while every original hold remains.
		current.Frozen = true
		return current, nil
	}
	current.HeldTokens -= heldTokens
	current.HeldCostMicroyuan -= heldCost
	current.KnownTokens += knownTokens
	current.KnownCostMicroyuan += knownCost
	if current.KnownTokens > MaxSafeInteger-current.HeldTokens || current.KnownCostMicroyuan > MaxSafeInteger-current.HeldCostMicroyuan {
		return Account{}, ErrInternal
	}
	current.Used.Tokens = current.KnownTokens + current.HeldTokens
	current.Used.CostMicroyuan = current.KnownCostMicroyuan + current.HeldCostMicroyuan
	return current, nil
}
