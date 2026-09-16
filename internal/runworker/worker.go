package runworker

import (
	"context"
	"errors"
	"slices"

	"github.com/xjfyrh/jobforge/internal/run"
	"github.com/xjfyrh/jobforge/internal/runexecutor"
	agentv1 "github.com/xjfyrh/jobforge/proto/jobforge/agent/v1"
)

var (
	// ErrCleanup forbids any further Claim when process cleanup is unconfirmed.
	ErrCleanup = errors.New("executor cleanup unconfirmed")
	// ErrAuthority indicates irreversible loss of local execution permission.
	ErrAuthority = errors.New("execution authority lost")
	// ErrBatchStopped requires a new explicit operational decision before work.
	ErrBatchStopped = errors.New("audited batch continuation stopped")
)

// Config is trusted deployment input. Credentials never enter manifest/frames.
type Config struct {
	Profiles     []run.Profile
	Environments map[string]runexecutor.Environment // Tenant-scoped read/provider keys.
}

// Worker holds capacity one; all scheduling decisions come from AgentService.
type Worker struct {
	client       agentv1.AgentServiceClient
	manifest     Manifest
	profiles     map[string]run.Profile
	environments map[string]runexecutor.Environment
}

// New validates the immutable profile/manifest relation before registration.
// The client must already carry deployment authentication and transport policy.
func New(client agentv1.AgentServiceClient, manifest Manifest, config Config) (*Worker, error) {
	if client == nil || manifest.Validate() != nil || len(config.Profiles) != len(manifest.Profiles) || len(config.Environments) < 1 || len(config.Environments) > 128 {
		return nil, run.ErrProfileUnavailable
	}
	w := &Worker{client: client, manifest: manifest, profiles: make(map[string]run.Profile), environments: make(map[string]runexecutor.Environment)}
	w.manifest.Profiles = slices.Clone(manifest.Profiles)
	for _, p := range config.Profiles {
		if _, err := manifest.profile(p.ID, p.Hash); err != nil || p.ExecutorVersion != manifest.ExecutorVersion || !run.ValidHash(p.Pricing.Hash) ||
			p.ValidateAuditPolicy() != nil || !p.AuditEnabled() || p.ExpectedResponseModel != "deepseek-flash" {
			return nil, run.ErrProfileUnavailable
		}
		if _, exists := w.profiles[p.ID]; exists {
			return nil, run.ErrProfileUnavailable
		}
		if _, err := run.ReservationBudget(p, run.SubcallChat); err != nil {
			return nil, run.ErrProfileUnavailable
		}
		p.Definition = slices.Clone(p.Definition)
		w.profiles[p.ID] = p
	}
	for tenant, environment := range config.Environments {
		if !run.ValidIdentifier(tenant) {
			return nil, run.ErrInvalidArgument
		}
		w.environments[tenant] = environment
	}
	return w, nil
}

// executionAuthority is implemented by the one attempt's lease keeper. Check
// reads current BOOTTIME authority; deadlines cannot be renewed by local I/O.
type executionAuthority interface {
	Check() error
	StepDeadline() int64
	Stop(reason string)
}

// stepOutcome never represents a local cursor decision. A missing Commit means
// the caller must not start another step in this attempt.
type stepOutcome struct {
	Commit    *agentv1.CommitStepResponse
	Failure   string
	Abandoned bool
	Fatal     error
}

// runStep owns one real guardian and original Conversation. Context cancellation
// revokes ordinary work; narrow captured metering has its own bounded cleanup.
func (w *Worker) runStep(ctx context.Context, lease *agentv1.RunLease, checkpoint *agentv1.Checkpoint, authority executionAuthority) stepOutcome {
	return w.coordinateStep(ctx, lease, checkpoint, authority)
}
