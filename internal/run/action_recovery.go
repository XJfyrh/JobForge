package run

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/xjfyrh/jobforge/internal/business"
)

// ReceiptReader performs one bounded, authenticated GET to the frozen receiver.
type ReceiptReader interface {
	LookupReceipt(context.Context, string, Profile, business.SignedAction) (business.ActionReceipt, bool, error)
}

// ReceiptQueryPermit owns one bounded tenant query lease.
type ReceiptQueryPermit struct {
	ID       string
	Deadline time.Time
}

// ActionRecoveryStore persists receipt-only recovery without model admission.
type ActionRecoveryStore interface {
	RecoveryAction(context.Context, string, string, bool) (ActionBinding, Profile, error)
	ReserveReceiptQuery(context.Context, string, string, string, string) (ReceiptQueryPermit, error)
	RecordReceiptQuery(context.Context, string, string, business.SignedAction, *business.ActionReceipt, string) error
	AdmitActionRetry(context.Context, Admission, business.SignedAction) (SubmitResponse, error)
}

// WithReceiptReader installs the independent receipt-only identity.
func WithReceiptReader(reader ReceiptReader) ServiceOption {
	return func(s *Service) { s.receiptReader = reader }
}

// ServiceOption installs a trusted optional service dependency.
type ServiceOption func(*Service)

func (s *Service) lookupActionReceipt(ctx context.Context, tenant, id, source string, binding ActionBinding, profile Profile) (ActionBinding, error) {
	if binding.Action == nil || binding.Effect.State == "applied" {
		return binding, nil
	}
	store, ok := s.store.(ActionRecoveryStore)
	if !ok || s.receiptReader == nil {
		return binding, ErrDependencyUnavailable
	}
	permit, err := store.ReserveReceiptQuery(ctx, tenant, id, source, uuid.NewString())
	if err != nil {
		return binding, err
	}
	lookupCtx, cancel := context.WithDeadline(ctx, permit.Deadline)
	receipt, found, lookupErr := s.receiptReader.LookupReceipt(lookupCtx, tenant, profile, *binding.Action)
	cancel()
	var accepted *business.ActionReceipt
	outcome := "absent"
	if lookupErr != nil {
		outcome = "unavailable"
	} else if found {
		if receipt.Validate(*binding.Action) != nil {
			lookupErr = ErrActionConflict
			outcome = "unavailable"
		} else {
			outcome = "found"
			accepted = &receipt
		}
	}
	// Cleanup has a separate bounded context; CAS release can never clear a
	// successor permit, including when the originating HTTP client disconnects.
	recordCtx, finish := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	recordErr := store.RecordReceiptQuery(recordCtx, tenant, permit.ID, *binding.Action, accepted, outcome)
	finish()
	if lookupErr != nil {
		return binding, lookupErr
	}
	if recordErr != nil {
		return binding, recordErr
	}
	if accepted != nil {
		binding.Effect.State = "applied"
		binding.Effect.Receipt = accepted
	}
	return binding, nil
}

// Reconcile only records effect facts; terminal execution facts remain immutable.
func (s *Service) Reconcile(ctx context.Context, tenant, id string) (EffectView, error) {
	store, ok := s.store.(ActionRecoveryStore)
	if !ok {
		return EffectView{}, ErrProfileUnavailable
	}
	binding, profile, err := store.RecoveryAction(ctx, tenant, id, false)
	if err != nil {
		return EffectView{}, err
	}
	if _, err := s.lookupActionReceipt(ctx, tenant, id, "reconcile", binding, profile); err != nil {
		return EffectView{}, err
	}
	return s.Effect(ctx, tenant, id)
}

func (s *Service) retryAction(ctx context.Context, input Admission) (*SubmitResponse, error) {
	store, ok := s.store.(ActionRecoveryStore)
	if !ok {
		return nil, nil
	}
	binding, profile, err := store.RecoveryAction(ctx, input.TenantID, input.SourceRunID, true)
	if err != nil {
		return nil, err
	}
	if binding.Action == nil {
		return nil, nil
	}
	if _, err := s.lookupActionReceipt(ctx, input.TenantID, input.SourceRunID, "retry", binding, profile); err != nil {
		return nil, err
	}
	input.RunID, input.OperationID, input.FirstStepID = s.newID(), s.newID(), s.newID()
	result, err := store.AdmitActionRetry(ctx, input, *binding.Action)
	if err != nil {
		return nil, err
	}
	return &result, nil
}
