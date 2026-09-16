package run

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
)

// PublicStore provides tenant-scoped reads and cancellation, independent of the
// external capture dependency used only for creating new execution identities.
type PublicStore interface {
	Get(context.Context, string, string) (Run, error)
	List(context.Context, string, ListFilter) (Page, error)
	Steps(context.Context, string, string, int64, int) (StepPage, error)
	Events(context.Context, string, string, int64, int) (EventPage, error)
	Result(context.Context, string, string) (Result, error)
	Cancel(context.Context, string, string, string) (CancelResponse, error)
}

// AdmissionStore owns short transactions. A failed insert is resolved through
// persisted identities; the service never retries a physical snapshot request.
type AdmissionStore interface {
	PublicStore
	ResolveAdmission(context.Context, string, string, string, string, string) (*SubmitResponse, error)
	AdmissionContext(context.Context, string, string, string, string) (Run, error)
	Profile(context.Context, string) (Profile, error)
	Admit(context.Context, Admission) (SubmitResponse, error)
	ResolveAdmissionFailure(context.Context, Admission, error) (SubmitResponse, error)
}

// Capturer is a trusted business adapter, not a model-selected URL or tool.
type Capturer interface {
	Capture(context.Context, string, string, string) (SnapshotBinding, error)
}

// Service owns admission concurrency and the single cross-database boundary.
// Its bounded per-tenant map comes from deployment, never untrusted requests.
type Service struct {
	PublicStore
	store    AdmissionStore
	capturer Capturer
	total    chan struct{}
	tenants  map[string]chan struct{}
	newID    func() string
}

// NewService creates the fixed admission limits: two captures per configured
// tenant and eight per process, without an unbounded waiting queue.
func NewService(store AdmissionStore, capturer Capturer, tenants []string) (*Service, error) {
	if store == nil || capturer == nil || len(tenants) == 0 {
		return nil, ErrInvalidArgument
	}
	s := &Service{PublicStore: store, store: store, capturer: capturer,
		total: make(chan struct{}, 8), tenants: make(map[string]chan struct{}, len(tenants)), newID: uuid.NewString}
	for _, tenant := range tenants {
		if !ValidIdentifier(tenant) {
			return nil, ErrInvalidArgument
		}
		s.tenants[tenant] = make(chan struct{}, 2)
	}
	return s, nil
}

func (s *Service) admissionSlot(tenant string) (func(), error) {
	slots, ok := s.tenants[tenant]
	if !ok {
		return nil, ErrNotFound
	}
	select {
	case slots <- struct{}{}:
	default:
		return nil, ErrQueueOverloaded
	}
	select {
	case s.total <- struct{}{}:
	default:
		<-slots
		return nil, ErrQueueOverloaded
	}
	var once sync.Once
	return func() { once.Do(func() { <-s.total; <-slots }) }, nil
}

// Submit resolves accepted identities before checking live dependencies or
// acquiring scarce capture capacity. Replays never extend deadlines or budgets.
func (s *Service) Submit(ctx context.Context, tenant, key string, request SubmitRequest) (SubmitResponse, error) {
	if !ValidIdentifier(tenant) || !ValidIdentifier(key) || request.Validate() != nil {
		return SubmitResponse{}, ErrInvalidArgument
	}
	hash := request.Hash()
	reused, err := s.store.ResolveAdmission(ctx, tenant, key, "", hash, request.BusinessRequestKey)
	if err != nil {
		return SubmitResponse{}, err
	}
	if reused != nil {
		return *reused, nil
	}
	return s.create(ctx, Admission{TenantID: tenant, OperationKey: key, RequestHash: hash, Submit: request})
}

// Retry creates at most one direct child and inherits every budget account and
// immutable profile. It obtains a new snapshot only for a genuinely new child.
func (s *Service) Retry(ctx context.Context, tenant, sourceID, key string, request RetryRequest) (SubmitResponse, error) {
	id, err := uuid.Parse(sourceID)
	if !ValidIdentifier(tenant) || !ValidIdentifier(key) || err != nil || id.String() != sourceID || request.Validate() != nil {
		return SubmitResponse{}, ErrInvalidArgument
	}
	hash := request.Hash(sourceID)
	reused, err := s.store.ResolveAdmission(ctx, tenant, key, sourceID, hash, "")
	if err != nil {
		return SubmitResponse{}, err
	}
	if reused != nil {
		return *reused, nil
	}
	return s.create(ctx, Admission{TenantID: tenant, OperationKey: key, RequestHash: hash,
		SourceRunID: sourceID, Retry: request})
}

func (s *Service) create(ctx context.Context, input Admission) (SubmitResponse, error) {
	release, err := s.admissionSlot(input.TenantID)
	if err != nil {
		return SubmitResponse{}, err
	}
	defer release()
	source, err := s.store.AdmissionContext(ctx, input.TenantID, input.SourceRunID, input.Submit.ProfileID, input.Submit.BudgetBatchID)
	if err != nil {
		return SubmitResponse{}, err
	}
	kind := "submit"
	if input.SourceRunID != "" {
		kind = "retry"
		input.Submit = SubmitRequest{SchemaVersion: 1, TicketID: source.TicketID, BusinessRequestKey: source.BusinessRequestKey,
			ProfileID: source.ProfileID, BudgetBatchID: source.BudgetBatchID, RunTimeoutSeconds: input.Retry.RunTimeoutSeconds}
	}
	input.Profile, err = s.store.Profile(ctx, input.Submit.ProfileID)
	if err != nil {
		return SubmitResponse{}, err
	}
	captureCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	input.Snapshot, err = s.capturer.Capture(captureCtx, input.TenantID, input.Submit.TicketID,
		SnapshotKey(input.TenantID, kind, input.OperationKey, input.SourceRunID))
	cancel()
	if err != nil {
		return SubmitResponse{}, err
	}
	input.RunID, input.BusinessID, input.FamilyAccountID = s.newID(), s.newID(), s.newID()
	input.OperationID, input.FirstStepID = s.newID(), s.newID()
	result, err := s.store.Admit(ctx, input)
	if err != nil {
		return s.store.ResolveAdmissionFailure(ctx, input, err)
	}
	return result, nil
}
