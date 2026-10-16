package runworker

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/xjfyrh/jobforge/internal/observability"
	"github.com/xjfyrh/jobforge/internal/run"
	"github.com/xjfyrh/jobforge/internal/run/businessclient"
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
	Actions      map[string]businessclient.ActionCredentials
}

// Worker holds capacity one; all scheduling decisions come from AgentService.
type Worker struct {
	client       agentv1.AgentServiceClient
	manifest     Manifest
	profiles     map[string]run.Profile
	environments map[string]runexecutor.Environment
	actions      *businessclient.ActionClient
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
		entry, err := manifest.profile(p.ID, p.Hash)
		if err != nil || p.ExecutorVersion != manifest.ExecutorVersion || !run.ValidHash(p.Pricing.Hash) ||
			p.ValidateAuditPolicy() != nil || run.ValidateSupportProfile(p) != nil || !p.AuditEnabled() || p.ExpectedResponseModel != "deepseek-flash" {
			return nil, run.ErrProfileUnavailable
		}
		if manifest.SchemaVersion == 2 || run.IsSupportStrategy(p.Strategy) {
			definition, err := run.DecodeSupportDefinition(p.Definition)
			if err != nil || (definition.SchemaVersion >= 7) != (manifest.SchemaVersion == 2) || manifest.SchemaVersion == 2 && entry.PromptVersion != definition.Program.PromptVersion {
				return nil, run.ErrProfileUnavailable
			}
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
	if len(config.Actions) > 0 {
		for _, identity := range config.Actions {
			for _, environment := range config.Environments {
				if identity.ReaderKey != "" && (identity.ReaderKey == environment.BusinessReadKey || identity.ReaderKey == environment.DeepSeekKey) ||
					identity.WriterKey != "" && (identity.WriterKey == environment.BusinessReadKey || identity.WriterKey == environment.DeepSeekKey) {
					return nil, run.ErrInvalidArgument
				}
			}
		}
		var err error
		w.actions, err = businessclient.NewActionClient(config.Actions)
		if err != nil {
			return nil, err
		}
	}
	for _, p := range config.Profiles {
		if p.ApprovalEnabled() && w.actions == nil {
			return nil, run.ErrProfileUnavailable
		}
		if p.ApprovalEnabled() {
			d, _ := run.DecodeSupportDefinition(p.Definition)
			for tenant := range config.Environments {
				i, ok := config.Actions[tenant]
				if !ok || i.WriterKey == "" || i.Origin != d.Action.Origin {
					return nil, run.ErrProfileUnavailable
				}
			}
		}
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
func (w *Worker) runStep(ctx context.Context, lease *agentv1.RunLease, checkpoint *agentv1.Checkpoint, authority executionAuthority) (outcome stepOutcome) {
	started := time.Now()
	defer func() {
		kind := "unknown"
		if checkpoint != nil && checkpoint.NextStep != nil {
			if name, exists := agentv1.StepKind_name[int32(checkpoint.NextStep.Kind)]; exists {
				kind = name
			}
		}
		result := "accepted"
		switch {
		case outcome.Fatal != nil:
			result = "fatal"
		case outcome.Abandoned:
			result = "abandoned"
		case outcome.Failure != "" || outcome.Commit == nil:
			result = "failed"
		}
		observability.RunStepDuration.WithLabelValues(kind, result).Observe(time.Since(started).Seconds())
	}()
	if checkpoint != nil && checkpoint.NextStep != nil && checkpoint.NextStep.Kind == agentv1.StepKind_STEP_KIND_APPLY_TICKET_RESOLUTION {
		return w.runAction(ctx, lease, checkpoint, authority)
	}
	return w.coordinateStep(ctx, lease, checkpoint, authority)
}
