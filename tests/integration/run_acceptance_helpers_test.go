package integration

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	agentrun "github.com/xjfyrh/jobforge/internal/run"
	runpostgres "github.com/xjfyrh/jobforge/internal/run/postgres"
)

// runHarness uses production admission, accounts, registration, and Claim on a
// real isolated PostgreSQL database. Only the business capture is a labelled
// deterministic fixture; it never serves as real model or business acceptance.
type runHarness struct {
	Ctx       context.Context
	Pool      *pgxpool.Pool
	Store     *runpostgres.Store
	Service   *agentrun.Service
	Profile   agentrun.Profile
	Session   agentrun.Session
	Principal string
	Capture   *runCaptureFixture
	Options   runpostgres.Options
}

type runCaptureFixture struct {
	mu          sync.Mutex
	requests    int
	unavailable bool
	snapshots   map[string]agentrun.SnapshotBinding
}

func (c *runCaptureFixture) Capture(_ context.Context, tenant, ticket, key string) (agentrun.SnapshotBinding, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requests++
	if c.unavailable {
		return agentrun.SnapshotBinding{}, agentrun.ErrDependencyUnavailable
	}
	scope := tenant + ":" + key
	if snapshot, ok := c.snapshots[scope]; ok {
		return snapshot, nil
	}
	snapshotID, indexID := uuid.NewString(), uuid.NewString()
	ticketJSON, _ := json.Marshal(map[string]any{"tenant_id": tenant, "ticket_id": ticket, "revision": 1,
		"observed_at": "2026-01-01T00:00:00Z", "order_id": "order-1", "policy_version": "fixture-policy-v1",
		"subject": "Synthetic contract ticket", "description": "Deterministic transport fixture", "status": "open"})
	vector, _ := json.Marshal(map[string]any{"schema_version": 1,
		"ticket":   map[string]any{"id": ticket, "revision": 1},
		"order":    map[string]any{"id": "order-1", "exists": true, "revision": 1},
		"delivery": map[string]any{"id": "delivery-1", "exists": true, "aggregate_revision": 1},
		"policy":   map[string]any{"version": "fixture-policy-v1", "revision": 1, "corpus_sha256": agentrun.Fingerprint("fixture-corpus")},
		"index":    map[string]any{"id": indexID, "profile_hash": agentrun.Fingerprint("fixture-index"), "content_hash": agentrun.Fingerprint("fixture-vectors")}})
	snapshot := agentrun.SnapshotBinding{TenantID: tenant, TicketID: ticket, ID: snapshotID,
		ContentHash: agentrun.Fingerprint("fixture-snapshot", snapshotID), Ticket: ticketJSON, VersionVector: vector,
		IndexID: indexID, IndexProfileHash: agentrun.Fingerprint("fixture-index")}
	c.snapshots[scope] = snapshot
	return snapshot, nil
}

func (c *runCaptureFixture) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.requests
}

func setupRunHarness(t *testing.T) *runHarness {
	t.Helper()
	ctx, pool := setupRunDB(t)
	profile := agentrun.Profile{ID: "contract-profile-v1", Hash: agentrun.Fingerprint("contract-profile-v1"),
		Strategy: agentrun.BoundedReadonlyStrategy, ExecutorVersion: "fixture-v1", Executable: true,
		MaxInputTokens: 100, MaxOutputTokens: 50, FamilyTokenLimit: 20000, FamilyCostMicroyuan: 2000000,
		Pricing: agentrun.Pricing{Hash: agentrun.Fingerprint("contract-price-v1"), Denominator: 1000,
			InputMissMicroyuan: 2000, InputHitMicroyuan: 40, OutputMicroyuan: 8000}, Definition: json.RawMessage(`{"fixture":true}`)}
	options := runpostgres.Options{Profiles: []agentrun.Profile{profile}, Workers: []agentrun.WorkerConfig{
		{ID: "contract-worker", Tenants: []string{"tenant-a", "tenant-b"}, ProfileIDs: []string{profile.ID}, Capacity: 2},
		{ID: "contract-worker-2", Tenants: []string{"tenant-a", "tenant-b"}, ProfileIDs: []string{profile.ID}, Capacity: 2},
	}, TenantCapacity: 2, ProfileCapacity: 2}
	store, err := runpostgres.New(pool, options)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureProfiles(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	limits := agentrun.Usage{Chat: 1200, LogicalTools: 800, QueryEmbedding: 800, ProfileMetadataHTTP: 1600,
		BusinessToolHTTP: 800, PhysicalHTTP: 4400, ProtocolCorrections: 100, Tokens: 2000000, CostMicroyuan: 200000000}
	batchID := uuid.NewString()
	if err := store.CreateBudget(ctx, agentrun.BudgetSpec{ID: batchID, Scope: "batch", Key: "contract-batch",
		ValidFrom: now.Add(-time.Hour), ValidUntil: now.Add(8 * 24 * time.Hour), Limits: limits}); err != nil {
		t.Fatal(err)
	}
	for _, tenant := range []string{"tenant-a", "tenant-b"} {
		tenantID := uuid.NewString()
		if err := store.CreateBudget(ctx, agentrun.BudgetSpec{ID: tenantID, Scope: "tenant", Key: tenant,
			ValidFrom: now.Add(-time.Hour), ValidUntil: now.Add(8 * 24 * time.Hour), Limits: limits}); err != nil {
			t.Fatal(err)
		}
		if err := store.BindBudgetTenant(ctx, tenant, batchID, tenantID); err != nil {
			t.Fatal(err)
		}
	}
	capture := &runCaptureFixture{snapshots: make(map[string]agentrun.SnapshotBinding)}
	service, err := agentrun.NewService(store, capture, []string{"tenant-a", "tenant-b"})
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Register(ctx, "contract-worker", uuid.NewString(), "fixture-v1")
	if err != nil {
		t.Fatal(err)
	}
	return &runHarness{Ctx: ctx, Pool: pool, Store: store, Service: service, Profile: profile, Session: session,
		Principal: "contract-worker", Capture: capture, Options: options}
}

func (h *runHarness) submit(t *testing.T, tenant, businessKey string) agentrun.Run {
	t.Helper()
	response, err := h.Service.Submit(h.Ctx, tenant, "submit-"+businessKey, agentrun.SubmitRequest{SchemaVersion: 1,
		TicketID: "ticket-1", BusinessRequestKey: businessKey, ProfileID: h.Profile.ID, BudgetBatchID: "contract-batch", RunTimeoutSeconds: 3600})
	if err != nil {
		t.Fatalf("production Run admission: %v", err)
	}
	if response.Reused || response.Run.State != agentrun.Ready || response.Run.Budget.Family.ID == "" {
		t.Fatalf("unexpected initial Run admission: reused=%t state=%s", response.Reused, response.Run.State)
	}
	return response.Run
}

func (h *runHarness) claim(t *testing.T) agentrun.ClaimedRun {
	t.Helper()
	claimed, err := h.Store.Claim(h.Ctx, h.Principal, h.Session.ID)
	if err != nil || claimed == nil {
		t.Fatalf("production Run Claim: present=%t error=%v", claimed != nil, err)
	}
	return *claimed
}

func currentRunStep(claimed agentrun.ClaimedRun) agentrun.StepIdentity {
	r, a := claimed.Checkpoint.Run, claimed.Checkpoint.Authority
	return agentrun.StepIdentity{ID: a.NextStepID, Sequence: r.CursorVersion + 1, CursorVersion: r.CursorVersion,
		Kind: a.NextStepKind, InputHash: a.NextInputHash, ProfileID: r.ProfileID, ProfileHash: r.ProfileHash,
		SnapshotID: r.SnapshotID, SnapshotHash: r.SnapshotHash}
}
